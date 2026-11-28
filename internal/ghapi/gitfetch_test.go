package ghapi

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/gitproto/pktline"
)

// The two commits of go-vcs/agentd#10. Each has the JSON git/commits answered,
// the raw object `git cat-file -p` printed, and what the git endpoint answered
// to the request fetchRequest builds (.fetch), all captured on 2026-11-28.
var goldens = []struct{ name, id string }{
	{"example-7f05d217", "7f05d217867b2af52b0a28c6d1c91df97e1b5b39"}, // unsigned, committer at -0700
	{"forge-4804a991", "4804a9911cfeadbedeaf0f6be9a1a6979016958b"},   // signed, +0100
}

func golden(t testing.TB, name, ext string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "commits", name+"."+ext))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenRawCommitsAreWhatTheirIdsSay(t *testing.T) {
	for _, g := range goldens {
		if id, _ := objcache.ID(40, objcache.Commit, golden(t, g.name, "raw")); id != g.id {
			t.Errorf("%s: the raw golden hashes to %s", g.name, id)
		}
	}
}

// The REST answer, once its mechanical faults are mended, rebuilds a signed
// commit exactly; an unsigned one only up to the offset the answer lost.
func TestDecodeCommitGoldens(t *testing.T) {
	raw := golden(t, "forge-4804a991", "raw")
	got, err := DecodeCommit(golden(t, "forge-4804a991", "json"))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("signed commit: %v\n%q\n%q", err, got, raw)
	}
	raw = golden(t, "example-7f05d217", "raw")
	got, err = DecodeCommit(golden(t, "example-7f05d217", "json"))
	if err != nil {
		t.Fatal(err)
	}
	// The answer dates the committer in UTC (the -0700 git hashed is not in it)
	// and its message has lost the newline git wrote.
	want := bytes.TrimSuffix(bytes.Replace(raw, []byte("1757967037 -0700"), []byte("1757967037 +0000"), 1), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("unsigned commit:\n%q\n%q", got, want)
	}
	if id, _ := objcache.ID(40, objcache.Commit, got); id == goldens[0].id {
		t.Fatal("the lossy rebuild hashes to the commit's id: the golden no longer shows the loss")
	}
}

func TestReadCommitPackGoldens(t *testing.T) {
	for _, g := range goldens {
		got, err := readCommitPack(bytes.NewReader(golden(t, g.name, "fetch")), g.id)
		if err != nil {
			t.Fatalf("%s: %v", g.name, err)
		}
		if want := golden(t, g.name, "raw"); !bytes.Equal(got, want) {
			t.Errorf("%s:\n%q\n%q", g.name, got, want)
		}
		if id, _ := objcache.ID(40, objcache.Commit, got); id != g.id {
			t.Errorf("%s: hashes to %s", g.name, id)
		}
	}
}

func TestFetchRequestIsWhatGitHubAnswered(t *testing.T) {
	if got, want := fetchRequest(goldens[0].id), golden(t, goldens[0].name, "request"); !bytes.Equal(got, want) {
		t.Fatalf("%q != %q", got, want)
	}
	if !bytes.Contains(fetchRequest(strings.Repeat("a", 64)), []byte("object-format=sha256")) {
		t.Fatal("a 64-digit id must ask for sha256")
	}
}

func gitServer(t *testing.T, status int, body []byte, seen *http.Request, seenBody *[]byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r
			*seenBody, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetGitURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestObjectCommitThroughGit(t *testing.T) {
	var req http.Request
	var body []byte
	c := gitServer(t, 200, golden(t, "example-7f05d217", "fetch"), &req, &body)
	raw, err := c.Object(context.Background(), "golang/example", "commit", goldens[0].id)
	if err != nil || !bytes.Equal(raw, golden(t, "example-7f05d217", "raw")) {
		t.Fatalf("%v %q", err, raw)
	}
	if req.Method != "POST" || req.URL.Path != "/golang/example.git/git-upload-pack" {
		t.Errorf("%s %s", req.Method, req.URL.Path)
	}
	if req.Header.Get("Git-Protocol") != "version=2" || !strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
		t.Errorf("headers %v", req.Header)
	}
	if strings.Contains(req.URL.String(), "tok") {
		t.Error("the token is in the URL")
	}
	if !bytes.Equal(body, golden(t, "example-7f05d217", "request")) {
		t.Errorf("request body %q", body)
	}
}

func TestObjectCommitThroughGitRefusals(t *testing.T) {
	ctx := context.Background()
	id := goldens[0].id
	pk := func(f func(*pktline.Writer)) []byte {
		var b bytes.Buffer
		f(pktline.NewWriter(&b))
		return b.Bytes()
	}
	for name, tc := range map[string]struct {
		status int
		body   []byte
		reason string
	}{
		"not our ref":   {200, pk(func(w *pktline.Writer) { w.Text("ERR upload-pack: not our ref " + id) }), aerr.NotFound},
		"other ERR":     {200, pk(func(w *pktline.Writer) { w.Text("ERR boom") }), aerr.Unavailable},
		"404":           {404, []byte("nope"), aerr.NotFound},
		"no packfile":   {200, pk(func(w *pktline.Writer) { w.Text("shallow-info"); w.Flush() }), aerr.Unavailable},
		"fatal band":    {200, pk(func(w *pktline.Writer) { w.Text("packfile"); w.Sideband(3, []byte("fatal")) }), aerr.Unavailable},
		"garbage":       {200, []byte("not a pkt-line"), aerr.Unavailable},
		"empty":         {200, nil, aerr.Unavailable},
		"truncated":     {200, golden(t, "example-7f05d217", "fetch")[:200], aerr.Unavailable},
		"server error":  {500, []byte("x"), aerr.Unavailable},
		"band 4":        {200, pk(func(w *pktline.Writer) { w.Text("packfile"); w.Sideband(4, []byte("x")) }), aerr.Unavailable},
		"empty payload": {200, pk(func(w *pktline.Writer) { w.Text("packfile"); w.Data(nil) }), aerr.Unavailable},
	} {
		c := gitServer(t, tc.status, tc.body, nil, nil)
		_, err := c.Object(ctx, "o/r", "commit", id)
		if err == nil || aerr.As(err).Reason != tc.reason {
			t.Errorf("%s: %v, want %v", name, err, tc.reason)
		}
	}
}

// onePack builds a pack of one object: the type and size header, the deflated
// body, the sha-1 trailer.
func onePack(typ byte, size int, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
	c := typ<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		b.WriteByte(c | 0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	b.WriteByte(c)
	zw := zlib.NewWriter(&b)
	zw.Write(body)
	zw.Close()
	h := sha1.Sum(b.Bytes())
	return append(b.Bytes(), h[:]...)
}

func TestParseCommitPackRefusals(t *testing.T) {
	body := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n\nm\n")
	good := onePack(1, len(body), body)
	if got, err := parseCommitPack(good, 40); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("a good pack: %v %q", err, got)
	}
	resum := func(p []byte) []byte {
		n := len(p) - 20
		h := sha1.Sum(p[:n])
		return append(p[:n:n], h[:]...)
	}
	mut := func(f func(p []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	for name, p := range map[string][]byte{
		"empty":          nil,
		"short":          good[:20],
		"bad magic":      mut(func(p []byte) []byte { p[0] = 'X'; return resum(p) }),
		"version 1":      mut(func(p []byte) []byte { p[7] = 1; return resum(p) }),
		"two objects":    mut(func(p []byte) []byte { p[11] = 2; return resum(p) }),
		"a tree":         onePack(2, len(body), body),
		"an ofs delta":   onePack(6, len(body), body),
		"a ref delta":    onePack(7, len(body), body),
		"bad checksum":   mut(func(p []byte) []byte { p[len(p)-1] ^= 1; return p }),
		"size too big":   onePack(1, len(body)+1, body),
		"size too small": onePack(1, len(body)-1, body),
		"trailing junk": mut(func(p []byte) []byte {
			return resum(append(p[:len(p)-20:len(p)-20], 'x', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
		}),
		"not zlib":  mut(func(p []byte) []byte { p[13] ^= 0xff; return resum(p) }),
		"huge size": onePack(1, MaxObject, nil),
	} {
		if got, err := parseCommitPack(p, 40); err == nil {
			t.Errorf("%s: accepted %q", name, got)
		}
	}
	if _, err := parseCommitPack(onePack(1, MaxObject, nil), 40); err != errBlobTooLarge {
		t.Errorf("a commit of MaxObject bytes: %v", err)
	}
}

func TestSetGitURLRefusals(t *testing.T) {
	c, _ := New("https://api.example.org", "tok")
	for _, u := range []string{"ftp://x", "http://example.org", "not a url", ""} {
		if err := c.SetGitURL(u); err == nil {
			t.Errorf("%q accepted", u)
		}
		if c.git != nil {
			t.Fatalf("%q left a git URL behind", u)
		}
	}
	if err := c.SetGitURL("http://127.0.0.1:1"); err != nil {
		t.Errorf("loopback http refused: %v", err)
	}
}

func TestObjectCommitThroughGitUnreachableAndCancelled(t *testing.T) {
	c := gitServer(t, 200, nil, nil, nil)
	srv := httptest.NewServer(http.NotFoundHandler())
	_ = c.SetGitURL(srv.URL)
	srv.Close() // nothing listens there any more
	if _, err := c.Object(context.Background(), "o/r", "commit", goldens[0].id); aerr.As(err).Reason != aerr.Unavailable {
		t.Errorf("unreachable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Object(ctx, "o/r", "commit", goldens[0].id); err != context.Canceled {
		t.Errorf("cancelled: %v", err)
	}
}

// rawPack makes a pack whose one entry starts with the given header bytes.
func rawPack(entry []byte) []byte {
	p := append([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01"), entry...)
	h := sha1.Sum(p)
	return append(p, h[:]...)
}

func TestParseCommitPackSizeHeader(t *testing.T) {
	// A size that continues past the end of the entry.
	if _, err := parseCommitPack(rawPack([]byte{0x90}), 40); err == nil {
		t.Error("a size header cut by the checksum accepted")
	}
	// A size of more than five bytes of varint.
	if _, err := parseCommitPack(rawPack([]byte{0x90, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01, 0x78}), 40); err == nil {
		t.Error("an over-long size accepted")
	}
	// A five-byte size is read in full: it is 2^25+1, over the bound, which is
	// what tells it from the over-long one above.
	if _, err := parseCommitPack(rawPack([]byte{0x91, 0x80, 0x80, 0x80, 0x01}), 40); err != errBlobTooLarge {
		t.Errorf("a five-byte size: %v", err)
	}
}

func TestReadCommitPackReadErrorIsReturned(t *testing.T) {
	// A response cut inside a packet is the reader's error, not a verdict on a
	// pack that was never assembled.
	if _, err := readCommitPack(strings.NewReader("0014packf"), strings.Repeat("a", 40)); err == nil || errors.Is(err, errPack) {
		t.Errorf("a cut response: %v", err)
	}
}

func FuzzReadCommitPack(f *testing.F) {
	for _, g := range goldens {
		f.Add(golden(f, g.name, "fetch"))
	}
	f.Add([]byte("0009ERR x\n"))
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01"))
	f.Fuzz(func(t *testing.T, in []byte) {
		raw, err := readCommitPack(bytes.NewReader(in), strings.Repeat("a", 40))
		if err == nil && len(raw) >= MaxObject {
			t.Fatalf("a commit of %d bytes accepted", len(raw))
		}
		raw, err = parseCommitPack(in, 40)
		if err == nil && len(raw) >= MaxObject {
			t.Fatalf("a commit of %d bytes accepted", len(raw))
		}
	})
}
