package ghapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const helperEnv = "GHAPI_KEYCHAIN_HELPER"

// TestKeychainHelper is the program the keychain command is replaced by: the
// test binary itself, told by the environment what to print.
func TestKeychainHelper(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("not a helper run")
	}
	switch mode {
	case "token":
		fmt.Println("ghp_secret123")
	case "spaces":
		fmt.Println("two words")
	case "empty":
	case "huge":
		fmt.Print(strings.Repeat("a", 1<<20))
	case "endless":
		for {
			if _, err := os.Stdout.WriteString(strings.Repeat("a", 4096)); err != nil {
				os.Exit(1)
			}
		}
	case "fail":
		fmt.Println("ghp_leaked_by_a_failing_lookup")
		os.Exit(44)
	case "hang":
		time.Sleep(time.Minute)
	case "args":
		// Prints its own argv: the item name must be there, the secret never is.
		fmt.Println(strings.Join(os.Args[1:], " "))
	}
	os.Exit(0)
}

// helper replaces the keychain command by the helper in the given mode.
func helper(t *testing.T, mode string) *[]string {
	t.Helper()
	var seen []string
	old := keychainCommand
	keychainCommand = func(service string) (string, []string, []string, error) {
		seen = append(seen, service)
		return os.Args[0], []string{"-test.run=^TestKeychainHelper$", "--", service}, []string{helperEnv + "=" + mode}, nil
	}
	t.Cleanup(func() { keychainCommand = old })
	return &seen
}

func TestKeychainTokenIsReadFromTheProgramsOutput(t *testing.T) {
	seen := helper(t, "token")
	tok, err := ReadKeychainToken(context.Background(), "forge-agentd.github")
	if err != nil || tok != "ghp_secret123" {
		t.Fatalf("%q %v", tok, err)
	}
	if len(*seen) != 1 || (*seen)[0] != "forge-agentd.github" {
		t.Fatalf("%v", *seen)
	}
}

func TestKeychainRefusals(t *testing.T) {
	for mode, want := range map[string]string{
		"spaces":  "not a single token",
		"empty":   "empty or not",
		"huge":    "larger than 1 KiB",
		"endless": "larger than 1 KiB",
		"fail":    "lookup of",
	} {
		t.Run(mode, func(t *testing.T) {
			helper(t, mode)
			tok, err := ReadKeychainToken(context.Background(), "svc")
			if err == nil || tok != "" || !strings.Contains(err.Error(), want) {
				t.Fatalf("%q %v, want %q", tok, err, want)
			}
			if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "two words") {
				t.Fatalf("the error carries what the program printed: %v", err)
			}
		})
	}
}

func TestKeychainLookupTimesOut(t *testing.T) {
	helper(t, "hang")
	old := keychainTimeout
	keychainTimeout = 200 * time.Millisecond
	defer func() { keychainTimeout = old }()
	start := time.Now()
	if _, err := ReadKeychainToken(context.Background(), "svc"); err == nil || !strings.Contains(err.Error(), "no answer in time") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the hung program was waited for")
	}
}

func TestKeychainCancelledContext(t *testing.T) {
	helper(t, "token")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadKeychainToken(ctx, "svc"); err == nil {
		t.Fatal("a cancelled context was ignored")
	}
}

func TestKeychainItemNames(t *testing.T) {
	seen := helper(t, "token")
	for _, bad := range []string{"", "-w", "-s", "a b", "a;b", "a/b", "a\x00b", "a\nb", "$(x)", strings.Repeat("a", 129), "é"} {
		if _, err := ReadKeychainToken(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "item name") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("a program ran for a refused name: %v", *seen)
	}
	for _, good := range []string{"a", "forge-agentd.github", "A_b-1.2", strings.Repeat("a", 128)} {
		if _, err := ReadKeychainToken(context.Background(), good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

func TestKeychainCommandShapes(t *testing.T) {
	bin, args, _, err := keychainCommand("svc")
	switch runtime.GOOS {
	case "darwin":
		if err != nil || bin != "/usr/bin/security" || strings.Join(args, " ") != "find-generic-password -s svc -w" {
			t.Fatalf("%q %v %v", bin, args, err)
		}
	case "linux":
		if err != nil || bin != "/usr/bin/secret-tool" || strings.Join(args, " ") != "lookup service svc" {
			t.Fatalf("%q %v %v", bin, args, err)
		}
	default:
		if err == nil || !strings.Contains(err.Error(), "no system keychain") {
			t.Fatalf("%v", err)
		}
	}
}

func TestKeychainWithoutACommandIsRefused(t *testing.T) {
	old := keychainCommand
	keychainCommand = func(string) (string, []string, []string, error) {
		return "/nonexistent/prog", nil, nil, errors.New("keychain: no system keychain on test")
	}
	defer func() { keychainCommand = old }()
	if tok, err := ReadKeychainToken(context.Background(), "svc"); err == nil || tok != "" || !strings.Contains(err.Error(), "no system keychain") {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestKeychainEnvIsMinimal(t *testing.T) {
	t.Setenv("HOME", "/home/x")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_pass")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/x")
	env := strings.Join(keychainBaseEnv(), "\n")
	if !strings.Contains(env, "HOME=/home/x") || !strings.Contains(env, "DBUS_SESSION_BUS_ADDRESS=") || strings.Contains(env, "GITHUB_TOKEN") {
		t.Fatalf("%q", env)
	}
}

func FuzzParseTokenAndService(f *testing.F) {
	for _, s := range []string{"", "ghp_x\n", "a b", "-w", "a\x00b", strings.Repeat("a", 2000)} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if tok, err := parseToken(b, "x"); err == nil {
			if tok == "" || len(tok) > maxToken {
				t.Fatalf("accepted %q", tok)
			}
			for i := 0; i < len(tok); i++ {
				if tok[i] <= ' ' || tok[i] > '~' {
					t.Fatalf("accepted %q", tok)
				}
			}
		}
		if validService(string(b)) && (len(b) == 0 || len(b) > maxService || b[0] == '-' || strings.ContainsAny(string(b), " /;\x00\n$")) {
			t.Fatalf("accepted name %q", b)
		}
	})
}

func TestParseTokenKeepsTheSourceInItsErrors(t *testing.T) {
	if _, err := parseToken([]byte(strings.Repeat("a", maxToken+1)), "keychain"); err == nil || !strings.HasPrefix(err.Error(), "keychain: ") {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"a\x00b", "a\x7fb", "tökén", "a\x01", "a\x1bb"} {
		if _, err := parseToken([]byte(bad), "keychain"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if tok, err := parseToken([]byte(" ghp_x\n"), "keychain"); err != nil || tok != "ghp_x" {
		t.Fatalf("%q %v", tok, err)
	}
}
