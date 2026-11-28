package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// fixture is a configuration whose binaries are the fakes of fakes_test.go.
func fixture(t *testing.T) (config, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness measures on POSIX machines: it reads uptime(1) and runs git from PATH")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	t.Cleanup(up.Close)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, n := range []string{"forge-agentd", "forge-agent", "git", "uptime"} {
		if err := os.Symlink(exe, filepath.Join(bin, n)); err != nil {
			t.Skip("no symlinks:", err)
		}
	}
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOMAXPROCS", "8") // the harness must set it to 1 itself
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("FAKE_STATE", state)
	t.Setenv("FAKE_TOKEN", token)
	t.Setenv("FAKE_REPO", "go-vcs/forge")
	t.Setenv("FAKE_COMMIT", testCommit)
	t.Setenv("FAKE_PATHS", "cmd,go.mod")
	t.Setenv("FAKE_LOADS", "1.5/2.5")
	for _, k := range []string{"FAKE_DAEMON", "FAKE_AGENT_FAIL", "FAKE_AGENT_SKIP", "FAKE_AGENT_LONG", "FAKE_GIT_FAIL", "FAKE_GIT_SKIP"} {
		t.Setenv(k, "")
	}
	c := config{
		agentd: filepath.Join(bin, "forge-agentd"), agent: filepath.Join(bin, "forge-agent"),
		repo: "go-vcs/forge", commit: testCommit, paths: []string{"cmd", "go.mod"},
		tokenFile: token, work: filepath.Join(root, "work"), out: filepath.Join(root, "out.json"),
		api: up.URL, gitURL: up.URL, rounds: 5, maxLoad: 8,
	}
	return c, state
}

func run(c config) (string, string, error) {
	var w, ew bytes.Buffer
	err := runBench(&w, &ew, c)
	return w.String(), ew.String(), err
}

func readResult(t *testing.T, path string) result {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func lines(t *testing.T, path string) int {
	t.Helper()
	b, _ := os.ReadFile(path)
	return len(strings.Fields(string(b)))
}

func TestRunBenchEndToEnd(t *testing.T) {
	c, state := fixture(t)
	table, log, err := run(c)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	r := readResult(t, c.out)
	if r.Repo != c.repo || r.Commit != c.commit || len(r.Paths) != 2 {
		t.Errorf("result header = %+v", r)
	}
	if len(r.Samples) != 25 || len(r.Loads) != 5 {
		t.Fatalf("%d samples, %d load rows; want 25 and 5", len(r.Samples), len(r.Loads))
	}
	// Each round starts from another group: agentd, git-full, git-sparse;
	// then git-full, git-sparse, agentd; then git-sparse, agentd, git-full.
	var order []string
	for _, s := range r.Samples[:15] {
		order = append(order, s.Variant)
	}
	want := "agentd-cold agentd-warm agentd-disk git-full git-sparse " +
		"git-full git-sparse agentd-cold agentd-warm agentd-disk " +
		"git-sparse agentd-cold agentd-warm agentd-disk git-full"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order of the first three rounds:\n got %s\nwant %s", got, want)
	}
	for _, s := range r.Samples {
		if s.Round < 1 || s.Round > 5 {
			t.Errorf("round %d", s.Round)
		}
		reqs, by, files, fb := 2, int64(20), 2, int64(10)
		switch s.Variant {
		case "git-full", "git-sparse":
			reqs, by, files, fb = 1, 10, 2, 5
		}
		if s.Requests != reqs || s.Bytes != by || s.Files != files || s.FileBytes != fb {
			t.Errorf("%+v: want %d requests, %d bytes, %d files, %d file bytes", s, reqs, by, files, fb)
		}
		if strings.HasPrefix(s.Variant, "agentd") && s.Seconds < 0.02 {
			t.Errorf("%s took %v s, less than the agent's own 20 ms", s.Variant, s.Seconds)
		}
	}
	for i, l := range r.Loads {
		if l.Attempt != i+1 || l.Round != i+1 || l.Dropped || l.Load1 != 1.5 || l.Load5 != 2.5 || l.After1 != 1.5 || l.After5 != 2.5 {
			t.Errorf("load row %d = %+v", i, l)
		}
	}
	last := -1
	for _, v := range variantOrder {
		i := strings.Index(table, "| "+v+" | 5 |")
		if i < 0 || i < last {
			t.Errorf("table has %s at %d after %d:\n%s", v, i, last, table)
		}
		last = i
	}
	if !strings.Contains(log, "round 5 (attempt 5): load 1.50 / 2.50") {
		t.Errorf("progress lacks the last round:\n%s", log)
	}
	if n := lines(t, filepath.Join(state, "starts")); n != 10 {
		t.Errorf("%d daemon starts, want 2 per round", n)
	}
	if n := lines(t, filepath.Join(state, "interrupts")); n != 10 {
		t.Errorf("%d daemon interrupts, want one per daemon", n)
	}
	if _, err := os.Stat(filepath.Join(c.work, "r1")); err == nil {
		t.Error("the round directory was left behind")
	}
}

func TestRunBenchWithoutOutFile(t *testing.T) {
	c, _ := fixture(t)
	c.out = ""
	c.rounds = 5
	if _, _, err := run(c); err != nil {
		t.Fatal(err)
	}
}

func TestRunBenchDropsAndRetries(t *testing.T) {
	c, _ := fixture(t)
	t.Setenv("FAKE_LOADS", "9/1 1/9 1/1 2/3")
	table, log, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "stopped after 0 of 5 rounds") {
		t.Fatalf("two drops in a row: %v", err)
	}
	if !strings.Contains(log, "attempt 2 dropped: load 1.00 / 9.00") || !strings.Contains(table, "| 2 | - | 1.00 / 9.00 | - | dropped |") {
		t.Errorf("drops not reported:\n%s\n%s", log, table)
	}
	if r := readResult(t, c.out); len(r.Loads) != 2 || !r.Loads[0].Dropped || r.Loads[0].Round != 0 {
		t.Errorf("a stopped run still reports what it has: %+v", r.Loads)
	}

	c, _ = fixture(t)
	t.Setenv("FAKE_LOADS", "9/1 1/1 2/3 2/3")
	if _, _, err := run(c); err != nil {
		t.Fatal(err)
	}
	r := readResult(t, c.out)
	if len(r.Loads) != 6 || !r.Loads[0].Dropped || r.Loads[1].Round != 1 || r.Loads[1].Attempt != 2 || r.Loads[1].After1 != 2 || r.Loads[1].After5 != 3 {
		t.Errorf("a dropped round is retried: %+v", r.Loads)
	}
}

func TestRunBenchLoadUnreadable(t *testing.T) {
	for _, spec := range []string{"garbage", "fail"} {
		c, state := fixture(t)
		t.Setenv("FAKE_LOADS", spec)
		_, _, err := run(c)
		if err == nil || !strings.Contains(err.Error(), "load could not be read") {
			t.Errorf("%s: %v", spec, err)
		}
		if _, err := os.Stat(filepath.Join(state, "starts")); err == nil {
			t.Errorf("%s: a round ran without a load", spec)
		}
	}
}

func TestRunBenchFailures(t *testing.T) {
	defer func() { listenTimeout, stopGrace = 20*time.Second, 10*time.Second }()
	for _, tc := range []struct{ name, key, val, want string }{
		{"cold agentd", "FAKE_AGENT_FAIL", "cold", "AGENTERR"},
		{"warm agentd", "FAKE_AGENT_FAIL", "warm", "AGENTERR"},
		{"disk agentd", "FAKE_AGENT_FAIL", "disk", "AGENTERR"},
		{"cold tree", "FAKE_AGENT_SKIP", "cold", "no such file"},
		{"warm tree", "FAKE_AGENT_SKIP", "warm", "no such file"},
		{"disk tree", "FAKE_AGENT_SKIP", "disk", "no such file"},
		{"git clone", "FAKE_GIT_FAIL", "clone", "GITERR"},
		{"git checkout", "FAKE_GIT_FAIL", "checkout", "GITERR"},
		{"git sparse", "FAKE_GIT_FAIL", "sparse-checkout", "GITERR"},
		{"git tree", "FAKE_GIT_SKIP", "1", "no such file"},
		{"daemon exits", "FAKE_DAEMON", "exit", "DAEMONERR"},
		{"daemon second start", "FAKE_DAEMON", "second", "DAEMONERR"},
		{"daemon speaks wrongly", "FAKE_DAEMON", "badline", "unexpected first line"},
	} {
		c, _ := fixture(t)
		listenTimeout, stopGrace = 10*time.Second, 200*time.Millisecond
		t.Setenv(tc.key, tc.val)
		_, _, err := run(c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
			continue
		}
		if !strings.Contains(tc.name, "daemon") && !strings.Contains(err.Error(), "round 1:") {
			t.Errorf("%s: the round is not named: %v", tc.name, err)
		}
	}
}

func TestRunBenchDaemonThatNeverListens(t *testing.T) {
	c, state := fixture(t)
	t.Setenv("FAKE_DAEMON", "hang")
	listenTimeout, stopGrace = 300*time.Millisecond, 200*time.Millisecond
	defer func() { listenTimeout, stopGrace = 20*time.Second, 10*time.Second }()
	start := time.Now()
	_, _, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "did not listen within 300ms") {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 8*time.Second {
		t.Errorf("took %v: the daemon is waited for, then killed", d)
	}
	if n := lines(t, filepath.Join(state, "starts")); n != 1 {
		t.Errorf("%d starts", n)
	}
}

func TestRunBenchSetupErrors(t *testing.T) {
	c, _ := fixture(t)
	c.rounds = 1
	if _, _, err := run(c); err == nil {
		t.Error("an invalid configuration ran")
	}
	c, _ = fixture(t)
	c.api = "http://[::1"
	if _, _, err := run(c); err == nil {
		t.Error("a bad API URL ran")
	}
	c, _ = fixture(t)
	c.gitURL = "http://[::1"
	if _, _, err := run(c); err == nil {
		t.Error("a bad git URL ran")
	}
	c, _ = fixture(t)
	c.work = filepath.Join(c.tokenFile, "work") // under a file
	if _, _, err := run(c); err == nil || strings.Contains(err.Error(), "round") {
		t.Errorf("a working directory under a file was made: %v", err)
	}
	c, _ = fixture(t)
	if err := os.MkdirAll(c.work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.work, "r1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(c); err == nil || !strings.Contains(err.Error(), "mkdir") || strings.Contains(err.Error(), "did not start") {
		t.Errorf("a round directory that cannot be made: %v", err)
	}
	c, _ = fixture(t)
	c.agentd = c.tokenFile // exists, is not executable
	if _, _, err := run(c); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("a daemon that cannot be started: %v", err)
	}
	c, _ = fixture(t)
	listenLoopback = func() (net.Listener, error) { return nil, errors.New("no port") }
	defer func() { listenLoopback = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") } }()
	if _, _, err := run(c); err == nil || err.Error() != "no port" {
		t.Errorf("a proxy that cannot listen: %v", err)
	}
	listenLoopback = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	c, _ = fixture(t)
	t.Setenv("FAKE_LOADS", "inf/1") // a load that cannot be written as JSON
	c.maxLoad = math.Inf(1)
	table, _, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "unsupported value") || !strings.Contains(table, "| agentd-cold |") {
		t.Errorf("a result that cannot be encoded: %v\n%s", err, table)
	}
	c, _ = fixture(t)
	c.out = filepath.Join(c.work, "no", "such", "dir", "out.json")
	if _, _, err := run(c); err == nil {
		t.Error("an unwritable result file was accepted")
	}
}

func TestTimedErrorKeepsTheTail(t *testing.T) {
	c, _ := fixture(t)
	t.Setenv("FAKE_AGENT_FAIL", "cold")
	t.Setenv("FAKE_AGENT_LONG", "1000")
	_, _, err := run(c)
	if err == nil || !strings.Contains(err.Error(), "forge-agent:") || !strings.Contains(err.Error(), "AGENTERR") {
		t.Fatalf("%v", err)
	}
	if strings.Count(err.Error(), "x") > 400 || !strings.Contains(err.Error(), "...x") {
		t.Errorf("the output is not cut to its last 400 bytes: %d x's", strings.Count(err.Error(), "x"))
	}
}

func TestTail(t *testing.T) {
	long := strings.Repeat("a", 100) + strings.Repeat("b", 400)
	if got := tail(long); got != "..."+strings.Repeat("b", 400) {
		t.Errorf("tail = %q", got)
	}
	exact := strings.Repeat("b", 400)
	if got := tail(exact); got != exact {
		t.Errorf("a 400-byte text was cut: %q", got)
	}
	if got := tail("short"); got != "short" {
		t.Errorf("tail = %q", got)
	}
}

func TestMainReportsAnError(t *testing.T) {
	code := -1
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"forge-bench"}
	main()
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
}
