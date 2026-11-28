package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/objcache"
)

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// gitIn runs git in dir with a fixed identity and no user or system config.
func gitIn(t *testing.T, dir, stdin string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1"), env...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %v: %v: %s", args, err, ee.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func ident(name, email string, unix int64, offSec int) commitIdent {
	return commitIdent{Name: name, Email: email, Date: time.Unix(unix, 0).In(time.FixedZone("", offSec)).Format(time.RFC3339)}
}

// A commit git itself wrote, rebuilt from the fields GitHub's JSON carries, is
// byte for byte what git hashed: the oracle is `git cat-file commit`.
func TestDecodeCommitEqualsGitsOwnBytes(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "", nil, "init", "-q")
	cases := []struct {
		name, msg string
		a, c      commitIdent
		aEnv      string
		cEnv      string
		parents   int
	}{
		{"plain", "subject\n\nbody line\n", ident("Ada Lovelace", "ada@example.org", 1700000000, 5400), ident("Bob", "bob@example.org", 1700000100, -3600*8), "1700000000 +0130", "1700000100 -0800", 0},
		{"utc", "x\n", ident("A", "a@x", 1700000000, 0), ident("A", "a@x", 1700000000, 0), "1700000000 +0000", "1700000000 +0000", 1},
		{"no trailing newline", "bare", ident("A", "a@x", 1000000000, 3600), ident("A", "a@x", 1000000001, 3600), "1000000000 +0100", "1000000001 +0100", 2},
		{"empty message", "", ident("A", "a@x", 1700000000, 0), ident("A", "a@x", 1700000000, 0), "1700000000 +0000", "1700000000 +0000", 0},
	}
	var parents []string
	for _, tc := range cases {
		args := []string{"commit-tree", emptyTree, "-F", "-"}
		var ps []string
		for i := 0; i < tc.parents; i++ {
			args = append(args, "-p", parents[i])
			ps = append(ps, parents[i])
		}
		env := []string{
			"GIT_AUTHOR_NAME=" + tc.a.Name, "GIT_AUTHOR_EMAIL=" + tc.a.Email, "GIT_AUTHOR_DATE=" + tc.aEnv,
			"GIT_COMMITTER_NAME=" + tc.c.Name, "GIT_COMMITTER_EMAIL=" + tc.c.Email, "GIT_COMMITTER_DATE=" + tc.cEnv,
		}
		// commit-tree adds a newline only when asked to; -F - keeps the message as is.
		id := gitIn(t, dir, tc.msg, env, args...)
		parents = append(parents, id)
		want := gitIn(t, dir, "", nil, "cat-file", "commit", id)

		var js commitReply
		js.Tree.SHA = emptyTree
		for _, p := range ps {
			js.Parents = append(js.Parents, struct {
				SHA string `json:"sha"`
			}{p})
		}
		js.Author, js.Committer, js.Message = tc.a, tc.c, tc.msg
		b, _ := json.Marshal(js)
		got, err := DecodeCommit(b)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// cat-file output is trimmed by gitIn; compare on the hash instead of bytes.
		if hid, _ := objcache.ID(40, objcache.Commit, got); hid != id {
			t.Errorf("%s: rebuilt commit hashes to %s, git wrote %s\n%q\n%q", tc.name, hid, id, got, want)
		}
	}
}

func TestDecodeCommitSigned(t *testing.T) {
	payload := "tree " + emptyTree + "\nauthor A <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n\nsigned message\n"
	sig := "-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n=abcd\n-----END PGP SIGNATURE-----"
	want := "tree " + emptyTree + "\nauthor A <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEE\n =abcd\n -----END PGP SIGNATURE-----\n \n" +
		"\nsigned message\n"
	b, _ := json.Marshal(map[string]any{"verification": map[string]any{"payload": payload, "signature": sig + "\n"}})
	got, err := DecodeCommit(b)
	if err != nil || string(got) != want {
		t.Fatalf("%v\n%q\n%q", err, got, want)
	}
	// Signature absent: the payload is the commit.
	b, _ = json.Marshal(map[string]any{"verification": map[string]any{"payload": payload, "signature": nil}})
	if got, err := DecodeCommit(b); err != nil || string(got) != payload {
		t.Fatalf("unsigned payload: %v %q", err, got)
	}
	// A payload with no message separator cannot take a signature header.
	b, _ = json.Marshal(map[string]any{"verification": map[string]any{"payload": "tree x", "signature": sig}})
	if _, err := DecodeCommit(b); err == nil {
		t.Fatal("payload without a separator accepted")
	}
}

func TestDecodeCommitRefusals(t *testing.T) {
	good := commitReply{Author: ident("A", "a@x", 1, 0), Committer: ident("A", "a@x", 1, 0), Message: "m"}
	good.Tree.SHA = emptyTree
	mutate := map[string]func(r *commitReply){
		"short tree": func(r *commitReply) { r.Tree.SHA = "abc" },
		"upper tree": func(r *commitReply) { r.Tree.SHA = strings.ToUpper(emptyTree) },
		"bad parent": func(r *commitReply) {
			r.Parents = append(r.Parents, struct {
				SHA string `json:"sha"`
			}{"zz"})
		},
		"newline in name": func(r *commitReply) { r.Author.Name = "A\ntree " + emptyTree },
		"angle in email":  func(r *commitReply) { r.Committer.Email = "a>x" },
		"nul in name":     func(r *commitReply) { r.Author.Name = "A\x00" },
		"bad date":        func(r *commitReply) { r.Author.Date = "yesterday" },
		"odd offset":      func(r *commitReply) { r.Committer.Date = "2023-11-14T22:13:20+00:00:30" },
		"too many parents": func(r *commitReply) {
			r.Parents = make([]struct {
				SHA string `json:"sha"`
			}, maxParents+1)
			for i := range r.Parents {
				r.Parents[i].SHA = emptyTree // each one valid: only the count is wrong
			}
		},
	}
	ok := good
	ok.Parents = make([]struct {
		SHA string `json:"sha"`
	}, maxParents)
	for i := range ok.Parents {
		ok.Parents[i].SHA = emptyTree
	}
	okJSON, _ := json.Marshal(ok)
	if _, err := DecodeCommit(okJSON); err != nil {
		t.Errorf("a commit with exactly maxParents parents was refused: %v", err)
	}
	// A type error in one field must refuse the commit, not rebuild the rest.
	typed := `{"tree":{"sha":"` + emptyTree + `"},"author":{"name":"A","email":"a@x","date":"2023-11-14T22:13:20Z"},"committer":{"name":"A","email":"a@x","date":"2023-11-14T22:13:20Z"},"message":5}`
	if _, err := DecodeCommit([]byte(typed)); err == nil {
		t.Error("a mistyped field was accepted")
	}
	for name, m := range mutate {
		r := good
		m(&r)
		b, _ := json.Marshal(r)
		if _, err := DecodeCommit(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := DecodeCommit([]byte("[")); err == nil {
		t.Error("bad json accepted")
	}
	big := good
	big.Message = strings.Repeat("x", MaxObject)
	b, _ := json.Marshal(big)
	if _, err := DecodeCommit(b); err != errBlobTooLarge {
		t.Errorf("oversize commit: %v", err)
	}
}

func TestObjectCommitAndItsRefusals(t *testing.T) {
	r := commitReply{Author: ident("A", "a@x", 1, 0), Committer: ident("A", "a@x", 1, 0), Message: "m\n"}
	r.Tree.SHA = emptyTree
	ok, _ := json.Marshal(r)
	bad := []byte(`{"author":{"date":"nope"}}`)
	c := objClient(t, map[string]string{
		"/repos/o/r/git/commits/" + helloID: string(ok),
		"/repos/o/r/git/commits/" + emptyID: string(bad),
	})
	ctx := context.Background()
	d, err := c.Object(ctx, "o/r", "commit", helloID)
	if err != nil || !bytes.HasPrefix(d, []byte("tree "+emptyTree+"\nauthor A <a@x> 1 +0000\n")) {
		t.Fatalf("%q %v", d, err)
	}
	if _, err := c.Object(ctx, "o/r", "commit", emptyID); aerr.As(err).Reason != aerr.Unavailable {
		t.Fatalf("unreadable commit: %v", err)
	}
	if _, err := c.Object(ctx, "o/r", "commit", strings.Repeat("2", 40)); aerr.As(err).Reason != aerr.NotFound {
		t.Fatalf("missing commit: %v", err)
	}
	big := r
	big.Message = strings.Repeat("x", MaxObject)
	bb, _ := json.Marshal(big)
	c = objClient(t, map[string]string{"/repos/o/r/git/commits/" + helloID: string(bb)})
	if _, err := c.Object(ctx, "o/r", "commit", helloID); aerr.As(err).Reason != aerr.BoundExceeded {
		t.Fatalf("oversize commit: %v", err)
	}
}

func FuzzDecodeCommit(f *testing.F) {
	r := commitReply{Author: ident("A", "a@x", 1, 3600), Committer: ident("A", "a@x", 1, 0), Message: "m"}
	r.Tree.SHA = emptyTree
	b, _ := json.Marshal(r)
	f.Add(b)
	f.Add([]byte(`{"verification":{"payload":"tree x\n\nm","signature":"s"}}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		raw, err := DecodeCommit(in)
		if err != nil {
			return
		}
		if len(raw) > MaxObject {
			t.Fatalf("a commit of %d bytes passed the bound", len(raw))
		}
	})
}
