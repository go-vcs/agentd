package ghapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/objcache"
)

const (
	helloID = "ce013625030ba8dba906f756967f9e9ca394464a"
	emptyID = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	// Created by `git mktree` from the two entries below.
	treeID   = "c2118dc8c1fbcdb66fdbd83bcbee94a45383eba5"
	treeJSON = `{"sha":"c2118dc8c1fbcdb66fdbd83bcbee94a45383eba5","truncated":false,"tree":[` +
		`{"path":"a.txt","mode":"100644","type":"blob","sha":"ce013625030ba8dba906f756967f9e9ca394464a"},` +
		`{"path":"z.sh","mode":"100755","type":"blob","sha":"e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"}]}`
)

func objClient(t *testing.T, routes map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := routes[r.URL.Path]; ok {
			_, _ = w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestObjectBlobAndTreeHashToTheirIds(t *testing.T) {
	c := objClient(t, map[string]string{
		"/repos/o/r/git/blobs/" + helloID: `{"content":"aGVsbG8K\n","encoding":"base64","size":6}`,
		"/repos/o/r/git/trees/" + treeID:  treeJSON,
	})
	b, err := c.Object(context.Background(), "o/r", "blob", helloID)
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("%q %v", b, err)
	}
	if id, _ := objcache.ID(40, objcache.Blob, b); id != helloID {
		t.Fatal("blob does not hash to its id")
	}
	raw, err := c.Object(context.Background(), "o/r", "tree", treeID)
	if err != nil {
		t.Fatal(err)
	}
	// The rebuilt raw tree is byte-for-byte what git hashed.
	if id, _ := objcache.ID(40, objcache.Tree, raw); id != treeID {
		t.Fatalf("rebuilt tree hashes to %s, git says %s", id, treeID)
	}
}

func TestObjectRefusals(t *testing.T) {
	big := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", MaxObject+1)))
	c := objClient(t, map[string]string{
		"/repos/o/r/git/blobs/" + helloID: `{"content":"aGVsbG8K","encoding":"base64","size":6}`,
		"/repos/o/r/git/blobs/" + emptyID: `{"content":"` + big + `","encoding":"base64","size":` + "2097153" + `}`,
		"/repos/o/r/git/trees/" + treeID:  `{"truncated":true,"tree":[]}`,
	})
	ctx := context.Background()
	for want, call := range map[string]func() error{
		aerr.AbbreviatedID: func() error { _, err := c.Object(ctx, "o/r", "blob", helloID[:7]); return err },
		aerr.Malformed:     func() error { _, err := c.Object(ctx, "o/r", "tag", helloID); return err },
	} {
		if err := call(); aerr.As(err).Reason != want {
			t.Errorf("want %s: %v", want, err)
		}
	}
	if _, err := c.Object(ctx, "o/../r", "blob", helloID); aerr.As(err).Reason != aerr.Malformed {
		t.Errorf("bad repo: %v", err)
	}
	if _, err := c.Object(ctx, "o/r", "blob", emptyID); aerr.As(err).Reason != aerr.BoundExceeded {
		t.Fatalf("oversize blob: %v", err)
	}
	if _, err := c.Object(ctx, "o/r", "tree", treeID); aerr.As(err).Reason != aerr.Unavailable {
		t.Fatalf("truncated tree: %v", err)
	}
	if _, err := c.Object(ctx, "o/r", "blob", strings.Repeat("0", 40)); aerr.As(err).Reason != aerr.NotFound {
		t.Fatalf("missing: %v", err)
	}
}

// Each answer below would decode to an EMPTY, valid-looking object if the
// error that the step reports were ignored: only that step's own check refuses.
func TestObjectUnreadableAnswersAreRefusedAtTheirOwnStep(t *testing.T) {
	c := objClient(t, map[string]string{
		"/repos/o/r/git/blobs/" + helloID: `{"encoding":"base64","content":"!!!","size":0}`,
		"/repos/o/r/git/blobs/" + emptyID: `{"encoding":"utf-8","content":"","size":"bad"}`,
		"/repos/o/r/git/trees/" + treeID:  `{"tree":[{"path":"a/b","mode":"100644","sha":"` + helloID + `"}]}`,
	})
	ctx := context.Background()
	for _, id := range []string{helloID, emptyID} {
		if d, err := c.Object(ctx, "o/r", "blob", id); aerr.As(err).Reason != aerr.Unavailable || d != nil {
			t.Errorf("blob %s: %q %v", id, d, err)
		}
		if _, err := DecodeBlob([]byte(map[string]string{
			helloID: `{"encoding":"base64","content":"!!!","size":0}`,
			emptyID: `{"encoding":"utf-8","content":"","size":"bad"}`,
		}[id]), 1024); err == nil {
			t.Errorf("DecodeBlob accepted the answer for %s", id)
		}
	}
	// A name the tree format cannot hold: DecodeTree passes it, Build refuses.
	if d, err := c.Object(ctx, "o/r", "tree", treeID); aerr.As(err).Reason != aerr.Unavailable || d != nil {
		t.Errorf("unbuildable tree: %q %v", d, err)
	}
	// An upstream failure keeps its own reason, it is not turned into a decode error.
	if _, err := c.Object(ctx, "o/r", "tree", strings.Repeat("1", 40)); aerr.As(err).Reason != aerr.NotFound {
		t.Errorf("missing tree: %v", err)
	}
}

func TestDecodeBlobAndTreeRefusals(t *testing.T) {
	for _, in := range []string{
		`{"encoding":"base64","content":"!!!","size":1}`,
		`{"encoding":"base64","content":"aGk=","size":99}`,
		`{"encoding":"base64","content":"aGk=","size":-1}`,
		`{"encoding":"rot13","content":"x","size":1}`,
		`{"encoding":"utf-8","content":"hi","size":5}`,
		`[`,
	} {
		if _, err := DecodeBlob([]byte(in), 1024); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
	if d, err := DecodeBlob([]byte(`{"encoding":"utf-8","content":"hi","size":2}`), 1024); err != nil || string(d) != "hi" {
		t.Fatal(err)
	}
	if _, err := DecodeBlob([]byte(`{"encoding":"base64","content":"`+strings.Repeat("A", 4000)+`","size":10}`), 10); !errors.Is(err, errBlobTooLarge) {
		t.Fatalf("content longer than the bound allows: %v", err)
	}
	for _, in := range []string{
		`{"tree":[{"path":"a","mode":"100666","sha":"` + helloID + `"}]}`,
		`{"truncated":true}`,
		`{`,
	} {
		if _, err := DecodeTree([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
	// "040000" is GitHub's spelling of git's "40000".
	es, err := DecodeTree([]byte(`{"tree":[{"path":"d","mode":"040000","sha":"` + helloID + `"}]}`))
	if err != nil || es[0].Mode != "40000" {
		t.Fatalf("%v %v", es, err)
	}
}

func FuzzDecodeBlob(f *testing.F) {
	f.Add([]byte(`{"content":"aGVsbG8K\n","encoding":"base64","size":6}`))
	f.Add([]byte(`{"content":"hi","encoding":"utf-8","size":2}`))
	f.Add([]byte(`{"content":"","encoding":"base64","size":-1}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := DecodeBlob(b, 4096)
		if err == nil && len(d) > 4096 {
			t.Fatalf("returned %d bytes over a 4096 bound", len(d))
		}
	})
}

func FuzzDecodeTree(f *testing.F) {
	f.Add([]byte(treeJSON))
	f.Add([]byte(`{"truncated":true,"tree":[]}`))
	f.Add([]byte(`{"tree":[{"path":"","mode":"","sha":""}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		es, err := DecodeTree(b)
		if err != nil {
			return
		}
		for _, e := range es {
			if _, ok := mustMode(e.Mode); !ok {
				t.Fatalf("non-canonical mode %q returned", e.Mode)
			}
		}
	})
}

func mustMode(m string) (string, bool) {
	switch m {
	case "100644", "100755", "40000", "120000", "160000":
		return m, true
	}
	return "", false
}
