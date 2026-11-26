package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-vcs/agentd/internal/aerr"
)

const (
	c1 = "abababababababababababababababababababab"
	c2 = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fakeGitHub answers as a green, open pull request 7 of o/r whose head is c1.
func fakeGitHub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/pulls/7/merge":
			_, _ = w.Write([]byte(`{"sha":"` + c2 + `","merged":true}`))
		case r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = w.Write([]byte(`{"state":"open","head":{"sha":"` + c1 + `"}}`))
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success","head_sha":"` + c1 + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func tokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte("ghp_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func startDaemon(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the daemon refuses to serve where there is no peer credential check")
	}
	sock := filepath.Join(shortDir(t), "d", "s")
	ctx, cancel := context.WithCancel(context.Background())
	out, errb := &syncBuf{}, &syncBuf{}
	c := DaemonCmd()
	c.SetOut(out)
	c.SetErr(errb)
	c.SetArgs([]string{"--socket", sock, "--token-file", tokenFile(t), "--api-url", fakeGitHub(t), "--poll-interval", "5ms"})
	done := make(chan error, 1)
	go func() { done <- c.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	for i := 0; i < 200; i++ {
		select {
		case err := <-done:
			t.Fatalf("daemon exited before listening: %v %s", err, errb.String())
		default:
		}
		if conn, err := net.Dial("unix", sock); err == nil {
			conn.Close()
			if !strings.Contains(out.String(), "listening on "+sock) {
				time.Sleep(10 * time.Millisecond)
			}
			return sock
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon never listened: %s", errb.String())
	return ""
}

func agent(t *testing.T, sock string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := AgentCmd()
	var o, e bytes.Buffer
	c.SetOut(&o)
	c.SetErr(&e)
	c.SetArgs(append([]string{"--socket", sock, "--label", "t"}, args...))
	survives(t, fmt.Sprint(args), func() { err = c.Execute() })
	return o.String(), e.String(), err
}

func code(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(exitError); ok {
		return ee.code
	}
	return -1
}

func TestEndToEnd(t *testing.T) {
	sock := startDaemon(t)

	out, _, err := agent(t, sock, "verdict", "o/r", c1)
	if code(err) != ExitGreen || !strings.Contains(out, c1) || !strings.Contains(out, "GREEN") {
		t.Errorf("verdict: %v %s", err, out)
	}
	out, _, err = agent(t, sock, "wait", "o/r", c1, "--timeout", "5s")
	if code(err) != ExitGreen || !strings.Contains(out, "GREEN") {
		t.Errorf("wait: %v %s", err, out)
	}
	// an abbreviation is refused, structured, with a failing code
	_, errs, err := agent(t, sock, "verdict", "o/r", c1[:7])
	if code(err) != ExitError || !strings.Contains(errs, `"reason":"ABBREVIATED_ID"`) {
		t.Errorf("abbrev: %v %s", err, errs)
	}

	id := strings.Repeat("0a", 16)
	out, errs, err = agent(t, sock, "merge", "o/r", "7", c1, "--request-id", id)
	if err != nil || !strings.Contains(out, c2) || errs != "" {
		t.Errorf("merge: %v out=%s err=%s", err, out, errs)
	}
	out2, _, err := agent(t, sock, "merge", "o/r", "7", c1, "--request-id", id)
	if err != nil || out2 != out {
		t.Errorf("replay differs: %v %q vs %q", err, out2, out)
	}
	// a generated id is printed so that a retry can reuse it
	_, errs, err = agent(t, sock, "merge", "o/r", "7", c1)
	if err != nil || !strings.HasPrefix(errs, "request-id ") || len(strings.TrimSpace(strings.TrimPrefix(errs, "request-id "))) != 32 {
		t.Errorf("generated id: %v %q", err, errs)
	}
	// the head is c1: naming c2 is a refusal naming the current commit
	_, errs, err = agent(t, sock, "merge", "o/r", "7", c2)
	if code(err) != ExitRed || !strings.Contains(errs, `"reason":"REF_MOVED"`) || !strings.Contains(errs, c1) {
		t.Errorf("moved: %v %s", err, errs)
	}
	if _, _, err := agent(t, sock, "merge", "o/r", "x", c1); err == nil {
		t.Error("a non-numeric pr was accepted")
	}
	if _, _, err := agent(t, sock, "merge", "o/r", "0", c1); err == nil {
		t.Error("pr 0 was accepted")
	}
}

func TestDaemonRefusals(t *testing.T) {
	gh := fakeGitHub(t)
	good := filepath.Join(shortDir(t), "d", "s")
	// A daemon that wrongly starts must not hang the suite: it is cut at 3 s.
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := DaemonCmd()
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		c.SetArgs(args)
		var err error
		survives(t, fmt.Sprint(args), func() { err = c.ExecuteContext(ctx) })
		return err
	}
	want := func(err error, sub string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), sub) {
			t.Errorf("want an error containing %q, got %v", sub, err)
		}
	}
	want(run("--socket", good, "--token-file", filepath.Join(t.TempDir(), "none"), "--api-url", gh), "token file")
	want(run("--socket", good, "--token-file", tokenFile(t), "--api-url", "ftp://x"), "scheme must be https")
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o600)
	if runtime.GOOS == "windows" {
		want(run("--socket", filepath.Join(f, "d", "s"), "--token-file", tokenFile(t), "--api-url", gh), "no peer credential check")
	} else {
		want(run("--socket", filepath.Join(f, "d", "s"), "--token-file", tokenFile(t), "--api-url", gh), "socket directory")
	}
	if err := run("--token-file", tokenFile(t), "extra"); err == nil {
		t.Error("a positional argument was accepted")
	}
}

func TestDefaultTokenFileWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // windows
	if got := defaultTokenFile(); got != "" {
		t.Errorf("defaultTokenFile without a home = %q", got)
	}
}

func TestAgentRefusals(t *testing.T) {
	sock := startDaemon(t)
	one := func(errs string) {
		t.Helper()
		if n := strings.Count(strings.TrimSpace(errs), "\n"); n != 0 || errs == "" {
			t.Errorf("want exactly one stderr line, got %q", errs)
		}
	}
	// wait with an abbreviation is a structured refusal
	_, errs, err := agent(t, sock, "wait", "o/r", c1[:7], "--timeout", "1s")
	if code(err) != ExitError || !strings.Contains(errs, `"reason":"ABBREVIATED_ID"`) {
		t.Errorf("wait abbrev: %v %s", err, errs)
	}
	one(errs)
	// verdict on a commit the upstream does not know about is a refusal, not green
	_, errs, err = agent(t, sock, "verdict", "o/r", c2)
	if code(err) == ExitGreen {
		t.Errorf("verdict of another commit: %v %s", err, errs)
	}
	// merge of an abbreviated id
	_, errs, err = agent(t, sock, "merge", "o/r", "7", c1[:7], "--request-id", strings.Repeat("0b", 16))
	if code(err) != ExitError || !strings.Contains(errs, `"reason":"ABBREVIATED_ID"`) {
		t.Errorf("merge abbrev: %v %s", err, errs)
	}
	// pr parse messages
	for _, pr := range []string{"x", "0"} {
		_, _, err = agent(t, sock, "merge", "o/r", pr, c1)
		if err == nil || !strings.Contains(err.Error(), "pr must be a positive number") {
			t.Errorf("pr %q: %v", pr, err)
		}
	}
	// no label
	c := AgentCmd()
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SetArgs([]string{"--socket", sock, "verdict", "o/r", c1})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "--label is required") {
		t.Errorf("no label: %v", err)
	}
	// unreachable daemon: one JSON line on stderr, exit 1
	dead := filepath.Join(shortDir(t), "none")
	for _, args := range [][]string{{"verdict", "o/r", c1}, {"wait", "o/r", c1}, {"merge", "o/r", "7", c1}} {
		_, errs, err = agent(t, dead, args...)
		if code(err) != ExitError {
			t.Errorf("%v: %v", args, err)
		}
		one(errs)
	}
}

func TestMainExitCodes(t *testing.T) {
	saved := osExit
	defer func() { osExit = saved }()
	got := -1
	osExit = func(c int) { got = c }
	for _, tc := range []struct {
		err  error
		want int
	}{{exitError{ExitRed}, ExitRed}, {exitError{ExitPending}, ExitPending}, {os.ErrNotExist, ExitError}, {nil, -1}} {
		got = -1
		c := AgentCmd()
		c.SilenceErrors = true
		c.RunE = func(*cobra.Command, []string) error { return tc.err }
		c.SetArgs(nil)
		Main(c)
		if got != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestReportMapsGateRefusalsToRed(t *testing.T) {
	for reason, want := range map[string]int{
		aerr.VerdictRed: ExitRed, aerr.VerdictPending: ExitRed, aerr.RefMoved: ExitRed,
		aerr.Conflict: ExitRed, aerr.ChangeClosed: ExitRed,
		aerr.Unavailable: ExitError, aerr.Malformed: ExitError, aerr.NotFound: ExitError,
	} {
		c := AgentCmd()
		var e bytes.Buffer
		c.SetErr(&e)
		if got := code(report(c, aerr.New(reason, "x"))); got != want {
			t.Errorf("%s: %d, want %d", reason, got, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	if !strings.HasSuffix(DefaultSocket(), filepath.Join("forge-agentd", "agentd.sock")) {
		t.Error(DefaultSocket())
	}
	if !strings.HasSuffix(defaultTokenFile(), ".github-token") {
		t.Error(defaultTokenFile())
	}
}
