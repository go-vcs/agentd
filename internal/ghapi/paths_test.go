package ghapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
)

func srvClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "t")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantMsg(t *testing.T, name string, err error, reason, msg string) {
	t.Helper()
	e := aerr.As(err)
	if err == nil || e.Reason != reason || !strings.Contains(e.Message, msg) {
		t.Errorf("%s: got %v (%s %q), want %s containing %q", name, err, e.Reason, e.Message, reason, msg)
	}
}

func TestReadTokenReasons(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadToken(filepath.Join(dir, "nope")); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing: %v", err)
	}
	if _, err := ReadToken(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("dir: %v", err)
	}
	p := filepath.Join(dir, "t")
	_ = os.WriteFile(p, []byte("a b"), 0o600)
	if _, err := ReadToken(p); err == nil || !strings.Contains(err.Error(), "not a single token") {
		t.Errorf("two words: %v", err)
	}
}

func TestNewReasons(t *testing.T) {
	for _, u := range []string{"", "api.github.com", "http://[::1"} {
		if _, err := New(u, "t"); err == nil || !strings.Contains(err.Error(), "not a URL") {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestTransportFailures(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c, _ := New(srv.URL, "t")
	srv.Close()
	_, err := c.PRHead(context.Background(), "o/r", 1)
	wantMsg(t, "unreachable", err, aerr.Unavailable, "unreachable")
	if !aerr.As(err).Retryable {
		t.Error("an unreachable upstream must be retryable")
	}

	c = srvClient(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.PRHead(ctx, "o/r", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}

	c = srvClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("abc"))
	})
	_, err = c.PRHead(context.Background(), "o/r", 1)
	wantMsg(t, "cut", err, aerr.Unavailable, "cut")

	c = srvClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) })
	_, err = c.PRHead(context.Background(), "o/r", 1)
	wantMsg(t, "unreadable", err, aerr.Unavailable, "unreadable")
}

func TestDecodersRefuse(t *testing.T) {
	if _, _, err := DecodeRuns([]byte("{")); err == nil {
		t.Error("runs")
	}
	if _, _, err := DecodeChecks([]byte("{")); err == nil {
		t.Error("checks")
	}
	for _, b := range []string{"{", `{}`, `{"total_count":null}`, `{"total_count":-1}`} {
		if _, err := DecodeJobCount([]byte(b)); err == nil {
			t.Errorf("job count %q accepted", b)
		}
	}
	if n, err := DecodeJobCount([]byte(`{"total_count":0}`)); err != nil || n != 0 {
		t.Errorf("explicit zero: %d %v", n, err)
	}
}

func TestVerdictUpstreamFailures(t *testing.T) {
	ok := `{"total_count":0,"workflow_runs":[]}`
	okChecks := `{"total_count":0,"check_runs":[]}`
	route := func(runs, checks string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/actions/runs"):
				_, _ = w.Write([]byte(runs))
			case strings.HasSuffix(r.URL.Path, "/check-runs"):
				if checks == "500" {
					w.WriteHeader(500)
					return
				}
				_, _ = w.Write([]byte(checks))
			}
		}
	}
	_, err := srvClient(t, route(`{"total_count":"x"}`, okChecks)).Verdict(context.Background(), "o/r", sha)
	wantMsg(t, "runs undecodable", err, aerr.Unavailable, "unreadable")
	_, err = srvClient(t, route(ok, "500")).Verdict(context.Background(), "o/r", sha)
	wantMsg(t, "checks 500", err, aerr.Unavailable, "upstream 500")
	_, err = srvClient(t, route(ok, `{"total_count":"x"}`)).Verdict(context.Background(), "o/r", sha)
	wantMsg(t, "checks undecodable", err, aerr.Unavailable, "unreadable")

	// a run about another commit is refused, never merged into the verdict
	other := strings.Repeat("cd", 20)
	_, err = srvClient(t, route(`{"total_count":1,"workflow_runs":[{"id":1,"workflow_id":1,"event":"push","status":"queued","head_sha":"`+other+`"}]}`, okChecks)).Verdict(context.Background(), "o/r", sha)
	wantMsg(t, "other commit", err, aerr.Unavailable, "another commit")

	// the job count of a passing run: a 500 on it is an error, not a count
	c, _ := newClient(t, files("runs_one_success.json", "checks_green.json", map[string]string{"11": "status:500"}))
	_, err = c.Verdict(context.Background(), "o/r", sha)
	wantMsg(t, "jobs 500", err, aerr.Unavailable, "upstream 500")
}

func TestChecksPaginationBound(t *testing.T) {
	var n atomic.Int32
	c := srvClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			return
		}
		n.Add(1)
		_, _ = w.Write([]byte(`{"total_count":100000,"check_runs":[{"id":1,"name":"a","status":"queued"}]}`))
	})
	c.maxPages = 3
	_, err := c.Verdict(context.Background(), "o/r", sha)
	if e := aerr.As(err); e.Reason != aerr.BoundExceeded || n.Load() != 3 {
		t.Errorf("err = %v after %d pages", err, n.Load())
	}
}

func TestBadRepoOrNumberSendsNothing(t *testing.T) {
	var n atomic.Int32
	c := srvClient(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1) })
	for _, tc := range []struct {
		repo string
		pr   int
	}{{"o/r", 0}, {"o/r", -1}, {"bad", 1}} {
		if _, err := c.PRHead(context.Background(), tc.repo, tc.pr); aerr.As(err).Reason != aerr.Malformed {
			t.Errorf("PRHead %+v: %v", tc, err)
		}
		if _, err := c.MergePR(context.Background(), tc.repo, tc.pr, sha, "merge"); aerr.As(err).Reason != aerr.Malformed {
			t.Errorf("MergePR %+v: %v", tc, err)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests were sent", n.Load())
	}
}

func TestMergeThatDidNotMergeIsAConflict(t *testing.T) {
	c := srvClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"merged":false,"sha":"x"}`)) })
	if _, err := c.MergePR(context.Background(), "o/r", 1, sha, "merge"); aerr.As(err).Reason != aerr.Conflict {
		t.Errorf("%v", err)
	}
}

func TestHead(t *testing.T) {
	body := func(s string) *Client {
		return srvClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(s)) })
	}
	if h, err := body(`{"state":"open","head":{"sha":"`+sha+`"}}`).Head(context.Background(), "o/r", 1); err != nil || h != sha {
		t.Errorf("open: %q %v", h, err)
	}
	for _, s := range []string{`{"state":"closed","head":{"sha":"` + sha + `"}}`, `{"state":"open","merged":true,"head":{"sha":"` + sha + `"}}`} {
		if _, err := body(s).Head(context.Background(), "o/r", 1); aerr.As(err).Reason != aerr.ChangeClosed {
			t.Errorf("%s: %v", s, err)
		}
	}
	nf := srvClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	if _, err := nf.Head(context.Background(), "o/r", 1); aerr.As(err).Reason != aerr.NotFound {
		t.Errorf("404: %v", err)
	}
}
