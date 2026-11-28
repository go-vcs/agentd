package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseLoad(t *testing.T) {
	for _, c := range []struct {
		in         string
		l1, l5, l3 float64
	}{
		{"13:49  up 16 days, 20:11, 1 user, load averages: 7.03 5.29 5.38\n", 7.03, 5.29, 5.38},
		{" 10:01:02 up 3 days,  2:11,  2 users,  load average: 0.52, 0.58, 0.59\n", 0.52, 0.58, 0.59},
	} {
		l1, l5, l15, err := parseLoad(c.in)
		if err != nil || l1 != c.l1 || l5 != c.l5 || l15 != c.l3 {
			t.Errorf("parseLoad(%q) = %v %v %v, %v", c.in, l1, l5, l15, err)
		}
	}
	for _, bad := range []string{"", "up 3 days", "load average", "load average 1 2 3", "load average: 1 2", "load average: 1 x 3", "load average: 1 -2 3", "load average: NaN 1 2"} {
		if _, _, _, err := parseLoad(bad); err == nil {
			t.Errorf("parseLoad(%q) was accepted", bad)
		}
	}
}

func TestGate(t *testing.T) {
	g := &gate{max: 8}
	step := func(l1, l5 float64, wantOK, wantStop bool) {
		t.Helper()
		if ok, stop := g.admit(l1, l5); ok != wantOK || stop != wantStop {
			t.Errorf("admit(%v, %v) = %v, %v; want %v, %v", l1, l5, ok, stop, wantOK, wantStop)
		}
	}
	step(8, 8, true, false)     // the bound itself is allowed
	step(8.01, 1, false, false) // the 1-minute load drops a round
	step(1, 1, true, false)     // a good round resets the count
	step(1, 8.01, false, false) // the 5-minute load drops a round
	step(9, 9, false, true)     // two in a row stop
}

func TestMedianExtent(t *testing.T) {
	if got := median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("odd median = %v", got)
	}
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("even median = %v", got)
	}
	if !math.IsNaN(median(nil)) {
		t.Error("the median of nothing is not NaN")
	}
	in := []float64{3, 1, 2}
	median(in)
	if !reflect.DeepEqual(in, []float64{3, 1, 2}) {
		t.Error("median sorted its argument")
	}
	if lo, hi := extent([]float64{3, 1, 2}); lo != 1 || hi != 3 {
		t.Errorf("extent = %v %v", lo, hi)
	}
	if lo, hi := extent(nil); !math.IsNaN(lo) || !math.IsNaN(hi) {
		t.Error("the extent of nothing is not NaN")
	}
}

// git's traffic is counted the way the daemon's is, by a proxy in front of the
// git host: GIT_TRACE_CURL_NO_DATA drops every "Recv data" line, so a trace
// cannot give the bytes.
func TestCloneURLIsThroughTheGitProxy(t *testing.T) {
	b := &bench{c: config{repo: "go-vcs/forge"}, g: &proxy{url: "http://127.0.0.1:9"}}
	if got, want := b.cloneURL(), "http://127.0.0.1:9/go-vcs/forge.git"; got != want {
		t.Errorf("cloneURL = %q, want %q", got, want)
	}
}

func TestBothProxiesAreSummed(t *testing.T) {
	a, g := &proxy{}, &proxy{}
	a.reqs.Store(3)
	a.bytes.Store(30)
	g.reqs.Store(1)
	g.bytes.Store(7)
	b := &bench{p: a, g: g}
	if r, n := b.upstream(); r != 4 || n != 37 {
		t.Errorf("upstream = %d, %d; want 4, 37", r, n)
	}
	b.resetCounts()
	if r, n := b.upstream(); r != 0 || n != 0 {
		t.Errorf("after reset %d, %d", r, n)
	}
}

func TestWalkTree(t *testing.T) {
	d := t.TempDir()
	for name, n := range map[string]int{"a": 3, "sub/b": 5, ".git/objects/x": 100} {
		p := filepath.Join(d, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, by, err := walkTree(d)
	if err != nil || files != 2 || by != 8 {
		t.Errorf("walkTree = %d, %d, %v; want 2 files, 8 bytes, the .git directory left out", files, by, err)
	}
	if _, _, err := walkTree(filepath.Join(d, "missing")); err == nil {
		t.Error("a missing directory was walked")
	}
}

// infoFails is a directory entry that lists but cannot be stat'ed, as a file
// removed during the walk does.
type infoFails struct{ fs.DirEntry }

func (infoFails) Info() (fs.FileInfo, error) { return nil, errors.New("gone") }

type listsBrokenEntry struct{ fstest.MapFS }

func (f listsBrokenEntry) ReadDir(name string) ([]fs.DirEntry, error) {
	es, err := f.MapFS.ReadDir(name)
	for i, e := range es {
		es[i] = infoFails{e}
	}
	return es, err
}

func TestWalkFSInfoError(t *testing.T) {
	fsys := listsBrokenEntry{fstest.MapFS{"a": {Data: []byte("abc")}}}
	if _, _, err := walkFS(fsys); err == nil || err.Error() != "gone" {
		t.Errorf("walkFS = %v, want the Info error", err)
	}
}

func TestSummarizeAndTable(t *testing.T) {
	ss := []sample{
		{Round: 1, Variant: "git-full", Seconds: 3, Requests: 4, Bytes: 100, Files: 9, FileBytes: 90},
		{Round: 2, Variant: "git-full", Seconds: 1, Requests: 6, Bytes: 300, Files: 9, FileBytes: 90},
		{Round: 3, Variant: "git-full", Seconds: 2, Requests: 5, Bytes: 200, Files: 9, FileBytes: 91},
		{Round: 1, Variant: "agentd-cold", Seconds: 0.5, Requests: 7, Bytes: 10, Files: 9, FileBytes: 90},
	}
	rows := summarize(ss)
	if len(rows) != 2 || rows[0].Variant != "git-full" || rows[1].Variant != "agentd-cold" {
		t.Fatalf("rows = %+v", rows)
	}
	g := rows[0]
	if g.N != 3 || g.Median != 2 || g.Lo != 1 || g.Hi != 3 || g.MedReq != 5 || g.MedBytes != 200 || g.ConsistentWithin {
		t.Errorf("git-full row = %+v", g)
	}
	if !rows[1].ConsistentWithin {
		t.Error("a single sample is consistent with itself")
	}
	var b bytes.Buffer
	writeTable(&b, rows, []loadRow{{Attempt: 1, Round: 1, Load1: 1, Load5: 2, After1: 3, After5: 4}, {Attempt: 2, Load1: 9, Load5: 9, Dropped: true}})
	for _, want := range []string{"| git-full | 3 | 2.00 | 1.00-3.00 | 5 | 200 | 9 (INCONSISTENT) | 90 |", "| agentd-cold | 1 | 0.50 |", "| 1 | 1 | 1.00 / 2.00 | 3.00 / 4.00 | |", "| 2 | - | 9.00 / 9.00 | - | dropped |"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, b.String())
		}
	}
	if rank("agentd-cold") >= rank("git-sparse") || rank("nope") != len(variantOrder) {
		t.Error("variant ranks are wrong")
	}
}

func TestSparsePatterns(t *testing.T) {
	got := sparsePatterns([]string{"cmd", "internal/gitview/", "go.mod"})
	if want := []string{"/cmd", "/internal/gitview", "/go.mod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sparsePatterns = %v", got)
	}
}

func TestProxyCounts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "1" {
			http.Error(w, "header lost", 400)
			return
		}
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer up.Close()
	p, err := startProxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", p.url+"/x", nil)
		req.Header.Set("X-Test", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "0123456789" || resp.StatusCode != 200 {
			t.Fatalf("proxied reply = %d %q", resp.StatusCode, b)
		}
	}
	if reqs, by := p.snapshot(); reqs != 3 || by != 30 {
		t.Errorf("snapshot = %d requests, %d bytes; want 3, 30", reqs, by)
	}
	p.reset()
	if reqs, by := p.snapshot(); reqs != 0 || by != 0 {
		t.Errorf("after reset: %d, %d", reqs, by)
	}
	if _, err := startProxy("http://[::1"); err == nil {
		t.Error("a bad target URL was accepted")
	}
}

func TestValidate(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok := config{agentd: f, agent: f, tokenFile: f, repo: "o/r", commit: strings.Repeat("a", 40), paths: []string{"x"}, work: d, rounds: 5}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*config){
		"rounds":   func(c *config) { c.rounds = 4 },
		"paths":    func(c *config) { c.paths = nil },
		"commit":   func(c *config) { c.commit = "main" },
		"repo":     func(c *config) { c.repo = "r" },
		"work":     func(c *config) { c.work = "rel" },
		"binary":   func(c *config) { c.agentd = filepath.Join(d, "none") },
		"tokenfil": func(c *config) { c.tokenFile = filepath.Join(d, "none") },
	} {
		c := ok
		mod(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: a bad configuration was accepted", name)
		}
	}
}

func TestRootCmd(t *testing.T) {
	run := func(args ...string) error {
		cmd := RootCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.Execute()
	}
	if err := run(); err == nil {
		t.Error("no flags were accepted")
	}
	d := t.TempDir()
	f := filepath.Join(d, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run("--agentd", f, "--agent", f, "--repo", "o/r", "--commit", "main", "--path", "x", "--token-file", f, "--work", d)
	if err == nil || !strings.Contains(err.Error(), "40-hex") {
		t.Errorf("a branch name was accepted as a commit: %v", err)
	}
}
