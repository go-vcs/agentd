package ghapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/verdict"
)

const sha = "abababababababababababababababababababab"

// fake serves recorded fixtures, keyed by "METHOD path" (query ignored except
// head_sha, which must be the full commit: like GitHub, anything shorter
// answers an EMPTY list rather than an error).
type fake struct {
	t     *testing.T
	files map[string]string
	mu    sync.Mutex
	log   []string
	auth  []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.RequestURI())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if hs := r.URL.Query().Get("head_sha"); hs != "" && hs != sha {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
		return
	}
	name, ok := f.files[r.Method+" "+r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "status:") {
		code := map[string]int{"409": 409, "405": 405, "500": 500, "403": 403}[strings.TrimPrefix(name, "status:")]
		w.WriteHeader(code)
		return
	}
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.t.Fatal(err)
	}
	_, _ = w.Write(b)
}

func newClient(t *testing.T, files map[string]string) (*Client, *fake) {
	t.Helper()
	f := &fake{t: t, files: files}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func files(runs, checks string, jobs map[string]string) map[string]string {
	m := map[string]string{
		"GET /repos/o/r/actions/runs":                   runs,
		"GET /repos/o/r/commits/" + sha + "/check-runs": checks,
	}
	for id, f := range jobs {
		m["GET /repos/o/r/actions/runs/"+id+"/jobs"] = f
	}
	return m
}

func TestVerdictTraps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  verdict.State
	}{
		{"red test lane among greens", files("runs_one_success.json", "checks_red_lane.json", map[string]string{"11": "jobs_3.json"}), verdict.Red},
		{"checks with conclusion null", files("runs_none.json", "checks_null.json", nil), verdict.Pending},
		{"runs that never started (zero jobs, failure)", files("runs_zero_jobs.json", "checks_green.json", nil), verdict.Red},
		{"success with zero jobs", files("runs_zero_jobs_success.json", "checks_green.json", map[string]string{"31": "jobs_0.json"}), verdict.Red},
		{"green with jobs counted", files("runs_one_success.json", "checks_green.json", map[string]string{"11": "jobs_3.json"}), verdict.Green},
		{"nothing at all", files("runs_none.json", "checks_none.json", nil), verdict.Pending},
	} {
		c, _ := newClient(t, tc.files)
		v, err := c.Verdict(context.Background(), "o/r", sha)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if v.State != tc.want || v.Commit != sha {
			t.Errorf("%s: %s @%s, want %s (%+v)", tc.name, v.State, v.Commit, tc.want, v.Checks)
		}
	}
}

// Trap: a 7-character prefix expanded into a fake SHA. The fake answers an
// empty, well-formed list for any head_sha but the real one, exactly as GitHub
// does; the client must refuse before asking at all.
func TestVerdictRefusesAbbreviationBeforeAnyRequest(t *testing.T) {
	c, f := newClient(t, files("runs_one_success.json", "checks_green.json", map[string]string{"11": "jobs_3.json"}))
	for id, want := range map[string]string{sha[:7]: aerr.AbbreviatedID, strings.ToUpper(sha): aerr.Malformed, "": aerr.Malformed} {
		_, err := c.Verdict(context.Background(), "o/r", id)
		if e := aerr.As(err); e.Reason != want {
			t.Errorf("%q: reason %q, want %q", id, e.Reason, want)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("a refused id reached the API: %v", f.log)
	}
	// The padded fake is a valid, different commit: GitHub's empty list must read
	// as PENDING, never as green.
	fakeSHA := sha[:7] + strings.Repeat("0", 33)
	f.files["GET /repos/o/r/commits/"+fakeSHA+"/check-runs"] = "checks_none.json"
	v, err := c.Verdict(context.Background(), "o/r", fakeSHA)
	if err != nil || v.State != verdict.Pending {
		t.Errorf("fake sha: %+v %v, want PENDING", v, err)
	}
}

func TestVerdictJobCountAbsentIsNotZero(t *testing.T) {
	c, _ := newClient(t, files("runs_one_success.json", "checks_green.json", map[string]string{"11": "jobs_nofield.json"}))
	_, err := c.Verdict(context.Background(), "o/r", sha)
	if e := aerr.As(err); e.Reason != aerr.Unavailable {
		t.Errorf("err = %v, want UNAVAILABLE", err)
	}
}

func TestVerdictRefusesBadRepo(t *testing.T) {
	c, f := newClient(t, nil)
	for _, r := range []string{"", "o", "o/", "../r", "o/..", "o/r/x", "o/r?x=1", "o r/x", strings.Repeat("a", 101) + "/r"} {
		if _, err := c.Verdict(context.Background(), r, sha); aerr.As(err).Reason != aerr.Malformed {
			t.Errorf("%q: %v", r, err)
		}
	}
	if len(f.log) != 0 {
		t.Errorf("requests made for invalid repos: %v", f.log)
	}
}

func TestTokenIsABearerHeaderOnly(t *testing.T) {
	c, f := newClient(t, files("runs_none.json", "checks_none.json", nil))
	if _, err := c.Verdict(context.Background(), "o/r", sha); err != nil {
		t.Fatal(err)
	}
	for i, line := range f.log {
		if strings.Contains(line, "secret-token") {
			t.Errorf("token in URL: %s", line)
		}
		if f.auth[i] != "Bearer secret-token" {
			t.Errorf("auth = %q", f.auth[i])
		}
	}
}

func TestResponseBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[],"pad":"` + strings.Repeat("x", 4096) + `"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t")
	c.maxResponse = 1024
	_, err := c.Verdict(context.Background(), "o/r", sha)
	if e := aerr.As(err); e.Reason != aerr.BoundExceeded {
		t.Errorf("err = %v, want BOUND_EXCEEDED", err)
	}
}

func TestPaginationBound(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write([]byte(`{"total_count":100000,"workflow_runs":[{"id":1,"workflow_id":1,"event":"push","run_number":1,"status":"queued","head_sha":"` + sha + `"}]}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t")
	c.maxPages = 3
	_, err := c.Verdict(context.Background(), "o/r", sha)
	if e := aerr.As(err); e.Reason != aerr.BoundExceeded || n != 3 {
		t.Errorf("err = %v after %d pages, want BOUND_EXCEEDED after 3", err, n)
	}
}

func TestStatusMapping(t *testing.T) {
	for code, want := range map[int]string{500: aerr.Unavailable, 403: aerr.NotPermitted, 404: aerr.NotFound, 409: aerr.RefMoved, 405: aerr.Conflict, 429: aerr.RateLimited, 422: aerr.Malformed} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		c, _ := New(srv.URL, "t")
		_, err := c.PRHead(context.Background(), "o/r", 1)
		if e := aerr.As(err); e.Reason != want {
			t.Errorf("%d: %v, want %s", code, err, want)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t")
	_, err := c.PRHead(context.Background(), "o/r", 1)
	if e := aerr.As(err); e.Reason != aerr.RateLimited || !e.Retryable || e.RetryAfterSec != 42 {
		t.Errorf("rate limit: %+v", e)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	other := 0
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other++
		if r.Header.Get("Authorization") != "" {
			t.Error("the token followed a redirect")
		}
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t")
	if _, err := c.PRHead(context.Background(), "o/r", 1); err == nil || other != 0 {
		t.Errorf("err=%v other=%d", err, other)
	}
}

func TestNewRefusesCleartextRemote(t *testing.T) {
	for _, u := range []string{"http://api.github.com", "ftp://x", "http://10.0.0.1:80", "api.github.com", ""} {
		if _, err := New(u, "t"); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	for _, u := range []string{"https://api.github.com", "http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1"} {
		if _, err := New(u, "t"); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	if _, err := New("https://api.github.com", ""); err == nil {
		t.Error("empty token accepted")
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, mode)
		return p
	}
	if tok, err := ReadToken(write("ok", "ghp_abc\n", 0o600)); err != nil || tok != "ghp_abc" {
		t.Errorf("ok: %q %v", tok, err)
	}
	for name, p := range map[string]string{
		"empty":     write("empty", "\n", 0o600),
		"two words": write("two", "a b", 0o600),
		"huge":      write("huge", strings.Repeat("a", 2000), 0o600),
		"dir":       dir,
		"missing":   filepath.Join(dir, "nope"),
	} {
		if _, err := ReadToken(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if runtime.GOOS != "windows" {
		_, err := ReadToken(write("open", "ghp_abc", 0o644))
		if err == nil || strings.Contains(err.Error(), "ghp_abc") {
			t.Errorf("group-readable token: %v", err)
		}
	}
}

func TestMergePR(t *testing.T) {
	m := map[string]string{"PUT /repos/o/r/pulls/5/merge": "merge_ok.json"}
	c, _ := newClient(t, m)
	got, err := c.MergePR(context.Background(), "o/r", 5, sha, "squash")
	if err != nil || got != strings.Repeat("cd", 20) {
		t.Errorf("%q %v", got, err)
	}
	for _, bad := range []struct {
		sha, method string
		pr          int
	}{{sha[:7], "merge", 5}, {sha, "force", 5}, {sha, "merge", 0}} {
		if _, err := c.MergePR(context.Background(), "o/r", bad.pr, bad.sha, bad.method); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	c2, _ := newClient(t, map[string]string{"PUT /repos/o/r/pulls/5/merge": "status:409"})
	if _, err := c2.MergePR(context.Background(), "o/r", 5, sha, "merge"); aerr.As(err).Reason != aerr.RefMoved {
		t.Errorf("409: %v", err)
	}
}

func TestPRHeadRefusesAbbreviatedHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"open","head":{"sha":"abababa"}}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t")
	if _, err := c.PRHead(context.Background(), "o/r", 1); err == nil || errors.Is(err, nil) && aerr.As(err).Reason != aerr.Unavailable {
		t.Errorf("err = %v", err)
	}
}
