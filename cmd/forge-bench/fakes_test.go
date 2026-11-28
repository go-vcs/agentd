package main

// The measured programs (forge-agentd, forge-agent, git, uptime) are replaced in
// the tests by this very test binary, linked under their names and put first on
// PATH: TestMain looks at the name it was started as and plays that program.
// The fakes check the arguments and the environment they are given, and fail
// or hang on request through FAKE_* variables, so that runBench is exercised end
// to end without a network, a git, or a GitHub token.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "forge-agentd":
		os.Exit(fakeAgentd(os.Args[1:]))
	case "forge-agent":
		os.Exit(fakeAgent(os.Args[1:]))
	case "git":
		os.Exit(fakeGit(os.Args[1:]))
	case "uptime":
		os.Exit(fakeUptime())
	}
	os.Exit(m.Run())
}

func flagVal(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func get(url string) {
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func fakeAgentd(args []string) int {
	if os.Getenv("GOMAXPROCS") != "1" {
		fmt.Fprintln(os.Stderr, "GOMAXPROCS is not 1")
		return 2
	}
	for _, f := range []string{"--socket", "--token-file", "--api-url", "--git-url", "--cache-dir", "--materialize-root"} {
		if flagVal(args, f) == "" {
			fmt.Fprintln(os.Stderr, "missing flag", f)
			return 2
		}
	}
	if flagVal(args, "--token-file") != os.Getenv("FAKE_TOKEN") {
		fmt.Fprintln(os.Stderr, "wrong token file")
		return 2
	}
	sock := flagVal(args, "--socket")
	dir := filepath.Dir(filepath.Dir(sock))
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return 2
	}
	state := os.Getenv("FAKE_STATE")
	appendLine(filepath.Join(state, "starts"), "start")
	switch os.Getenv("FAKE_DAEMON") {
	case "badline":
		fmt.Println("hello")
		time.Sleep(time.Hour)
	case "exit":
		fmt.Fprintln(os.Stderr, "DAEMONERR")
		return 1
	case "hang":
		signal.Ignore(os.Interrupt)
		time.Sleep(time.Hour)
	case "second":
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			fmt.Fprintln(os.Stderr, "DAEMONERR")
			return 1
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "started"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "urls"), []byte(flagVal(args, "--api-url")+"\n"+flagVal(args, "--git-url")+"\n"), 0o600)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	fmt.Println("listening on", sock)
	<-ch
	appendLine(filepath.Join(state, "interrupts"), "int")
	return 0
}

func fakeAgent(args []string) int {
	if os.Getenv("GOMAXPROCS") != "1" {
		fmt.Fprintln(os.Stderr, "GOMAXPROCS is not 1")
		return 2
	}
	paths := strings.Split(os.Getenv("FAKE_PATHS"), ",")
	if len(args) != 8+2*len(paths) || args[0] != "--socket" || args[2] != "--label" || args[3] != "bench" ||
		args[4] != "materialize" || args[5] != os.Getenv("FAKE_REPO") || args[6] != os.Getenv("FAKE_COMMIT") {
		fmt.Fprintln(os.Stderr, "bad arguments", args)
		return 2
	}
	for i, p := range paths {
		if args[8+2*i] != "--path" || args[9+2*i] != p {
			fmt.Fprintln(os.Stderr, "bad path arguments", args)
			return 2
		}
	}
	name := args[7]
	if long := os.Getenv("FAKE_AGENT_LONG"); long != "" {
		n, _ := strconv.Atoi(long)
		fmt.Fprint(os.Stderr, strings.Repeat("x", n), "ENDMARK")
	}
	if os.Getenv("FAKE_AGENT_FAIL") == name {
		fmt.Fprintln(os.Stderr, "AGENTERR")
		return 1
	}
	time.Sleep(20 * time.Millisecond)
	sock := args[1]
	dir := filepath.Dir(filepath.Dir(sock))
	if b, err := os.ReadFile(filepath.Join(dir, "urls")); err == nil {
		for _, u := range strings.Fields(string(b)) {
			get(u + "/x")
		}
	}
	if os.Getenv("FAKE_AGENT_SKIP") == name {
		return 0
	}
	for _, p := range paths {
		f := filepath.Join(dir, "mat", name, p)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return 2
		}
		if err := os.WriteFile(f, []byte("12345"), 0o644); err != nil {
			return 2
		}
	}
	return 0
}

func fakeGit(args []string) int {
	if os.Getenv("GIT_CONFIG_GLOBAL") != os.DevNull || os.Getenv("GIT_CONFIG_NOSYSTEM") != "1" || os.Getenv("GIT_TERMINAL_PROMPT") != "0" {
		fmt.Fprintln(os.Stderr, "git runs with the user's configuration")
		return 2
	}
	for _, t := range gitTune {
		if len(args) == 0 || args[0] != t {
			fmt.Fprintln(os.Stderr, "git is not tuned to one thread", args)
			return 2
		}
		args = args[1:]
	}
	dest := ""
	if args[0] == "-C" {
		dest, args = args[1], args[2:]
	}
	sub := args[0]
	if os.Getenv("FAKE_GIT_FAIL") == sub {
		fmt.Fprintln(os.Stderr, "GITERR")
		return 1
	}
	if os.Getenv("FAKE_GIT_SKIP") != "" {
		return 0
	}
	write := func(name, s string) int {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return 2
		}
		if err := os.WriteFile(filepath.Join(dest, name), []byte(s), 0o644); err != nil {
			return 2
		}
		return 0
	}
	switch sub {
	case "clone":
		dest = args[len(args)-1]
		get(args[len(args)-2] + "/info/refs")
		return write("f", "abc")
	case "checkout":
		return write("c", "xy")
	case "sparse-checkout":
		if args[1] != "set" || args[2] != "--no-cone" || args[3] != "/"+strings.Split(os.Getenv("FAKE_PATHS"), ",")[0] {
			fmt.Fprintln(os.Stderr, "bad sparse-checkout", args)
			return 2
		}
	}
	return 0
}

// fakeUptime prints the loads of FAKE_LOADS, "l1/l5 l1/l5 ...": the n-th call
// prints the n-th pair, and the last one repeats. "garbage" prints no load.
func fakeUptime() int {
	if os.Getenv("LC_ALL") != "C" {
		fmt.Println("up 1 day, loads in a local format")
		return 0
	}
	spec := os.Getenv("FAKE_LOADS")
	if spec == "garbage" {
		fmt.Println("13:49  up 16 days")
		return 0
	}
	if spec == "fail" { // a load that is printed, and a command that failed
		fmt.Println("13:49  up 16 days, 20:11, 1 user, load averages: 0.10 0.10 0.10")
		return 1
	}
	pairs := strings.Fields(spec)
	n := filepath.Join(os.Getenv("FAKE_STATE"), "uptimes")
	b, _ := os.ReadFile(n)
	calls := len(strings.Fields(string(b)))
	appendLine(n, "call")
	i := min(calls, len(pairs)-1)
	l := strings.Split(pairs[i], "/")
	fmt.Printf("13:49  up 16 days, 20:11, 1 user, load averages: %s %s 3.00\n", l[0], l[1])
	return 0
}
