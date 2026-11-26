package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
)

const commit = "abababababababababababababababababababab"

// stub serves h on a unix socket (short path: sun_path is 104 bytes on macOS).
func stub(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	p := filepath.Join(d, "s")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skip("no unix sockets here:", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return p
}

func TestRequestIDsAreDistinctAndValid(t *testing.T) {
	a := NewRequestID()
	b := NewRequestID()
	if a == b || len(a) != 32 {
		t.Errorf("%q %q", a, b)
	}
}

func TestHelloThenVerdict(t *testing.T) {
	sess := "s-" + strings.Repeat("ab", 16)
	c := New(stub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_, _ = w.Write([]byte(`{"session":"` + sess + `","label":"x"}`))
		case "/v1/verdict":
			b := make([]byte, 400)
			n, _ := r.Body.Read(b)
			if !strings.Contains(string(b[:n]), sess) {
				t.Errorf("the session was not sent: %s", b[:n])
			}
			_, _ = w.Write([]byte(`{"commit":"` + commit + `","state":"GREEN","checks":[]}`))
		}
	}), "x")
	if err := c.Hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := c.Verdict(context.Background(), "o/r", commit)
	if err != nil || v.Commit != commit || v.State != "GREEN" {
		t.Errorf("%+v %v", v, err)
	}
}

func TestFailures(t *testing.T) {
	ctx := context.Background()
	// nobody listens
	if err := New(filepath.Join(os.TempDir(), "agentd-nobody-here.sock"), "x").Hello(ctx); aerr.As(err).Reason != aerr.Unavailable {
		t.Errorf("unreachable: %v", err)
	}
	// a structured refusal comes back as itself, from Hello and from a read
	c := New(stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"reason":"MALFORMED","retryable":false,"message":"no"}}`))
	}), "x")
	if err := c.Hello(ctx); aerr.As(err).Reason != aerr.Malformed {
		t.Errorf("hello refusal: %v", err)
	}
	if _, err := c.Merge(ctx, "id", "o/r", 1, commit, "merge"); aerr.As(err).Reason != aerr.Malformed {
		t.Errorf("merge refusal: %v", err)
	}
	// an error status without a structured body is INTERNAL
	for _, body := range []string{"boom", `{"error":null}`, `{}`} {
		c = New(stub(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(body))
		}), "x")
		if _, err := c.Verdict(ctx, "o/r", commit); aerr.As(err).Reason != aerr.Internal {
			t.Errorf("%q: %v", body, err)
		}
	}
	// a reply larger than the bound is refused, not buffered
	c = New(stub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("a", MaxReply) + `"}`))
	}), "x")
	if _, err := c.Verdict(ctx, "o/r", commit); aerr.As(err).Reason != aerr.Unavailable || !strings.Contains(aerr.As(err).Message, "too large") {
		t.Errorf("large: %v", err)
	}
	// a cut reply is unreadable
	c = New(stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("abc"))
	}), "x")
	if _, err := c.Verdict(ctx, "o/r", commit); aerr.As(err).Reason != aerr.Unavailable {
		t.Errorf("cut: %v", err)
	}
	// 200 with a body that is not the reply
	c = New(stub(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("nope")) }), "x")
	if _, err := c.Verdict(ctx, "o/r", commit); err == nil {
		t.Error("garbage 200 accepted")
	}
}

func TestWaitSendsWholeSeconds(t *testing.T) {
	var got string
	c := New(stub(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 300)
		n, _ := r.Body.Read(b)
		got = string(b[:n])
		_, _ = w.Write([]byte(`{"commit":"` + commit + `","state":"PENDING","checks":[],"timed_out":true}`))
	}), "x")
	r, err := c.Wait(context.Background(), "o/r", commit, 90*time.Second)
	if err != nil || !r.TimedOut || !strings.Contains(got, `"timeout_sec":90`) {
		t.Errorf("%+v %v %s", r, err, got)
	}
}
