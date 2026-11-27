package gittree

import (
	"bytes"
	"strings"
	"testing"
)

const id20 = "0123456789abcdef0123456789abcdef01234567"

func TestRoundTrip(t *testing.T) {
	in := []Entry{
		{"100644", "a.txt", id20},
		{"40000", "dir", id20},
		{"120000", "ln", id20},
		{"160000", "sub", id20},
		{"100755", "x", id20},
	}
	raw, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(raw, 20)
	if err != nil || len(out) != len(in) {
		t.Fatalf("parse: %v %v", out, err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("entry %d: %v != %v", i, out[i], in[i])
		}
	}
	again, _ := Build(out)
	if !bytes.Equal(raw, again) {
		t.Fatal("not stable")
	}
	kinds := []string{"blob", "tree", "blob", "commit", "blob"}
	for i, e := range out {
		if e.Kind() != kinds[i] {
			t.Fatalf("kind %d: %s", i, e.Kind())
		}
	}
}

func TestSHA256Ids(t *testing.T) {
	raw, err := Build([]Entry{{"100644", "f", strings.Repeat("ab", 32)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw, 20); err == nil {
		t.Fatal("a 32-byte id read as 20 must not parse")
	}
	if _, err := Parse(raw, 7); err == nil {
		t.Fatal("bad id length accepted")
	}
	// Input that would parse with a 7-byte id, and the empty input, which has
	// no record to disagree with the length: only the length guard refuses.
	seven := []byte("100644 f\x00abcdefg")
	for _, n := range []int{0, 7, 19, 21, 31, 33} {
		if _, err := Parse(seven, n); err == nil {
			t.Fatalf("id length %d accepted", n)
		}
		if _, err := Parse(nil, n); err == nil {
			t.Fatalf("id length %d accepted on empty input", n)
		}
	}
	for _, n := range []int{20, 32} {
		if out, err := Parse(nil, n); err != nil || len(out) != 0 {
			t.Fatalf("empty tree with id length %d: %v %v", n, out, err)
		}
	}
}

func TestRefusals(t *testing.T) {
	bad := map[string]string{
		"no space":      "100644",
		"zero mode":     "100666 f\x00" + strings.Repeat("a", 20),
		"empty name":    "100644 \x00" + strings.Repeat("a", 20),
		"dot":           "100644 .\x00" + strings.Repeat("a", 20),
		"dotdot":        "100644 ..\x00" + strings.Repeat("a", 20),
		"slash":         "100644 a/b\x00" + strings.Repeat("a", 20),
		"short id":      "100644 f\x00" + strings.Repeat("a", 19),
		"no nul":        "100644 f",
		"long name":     "100644 " + strings.Repeat("n", MaxName+1) + "\x00" + strings.Repeat("a", 20),
		"padded mode":   "040000 d\x00" + strings.Repeat("a", 20),
		"long mode":     "1006440 f\x00" + strings.Repeat("a", 20),
		"leading space": " f\x00" + strings.Repeat("a", 20),
	}
	for name, raw := range bad {
		if _, err := Parse([]byte(raw), 20); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Build refuses what Parse would.
	for _, e := range []Entry{{"100644", "", id20}, {"bad", "f", id20}, {"100644", "f", "zz"}, {"100644", "f", "ab"}, {"100644", "a/b", id20}} {
		if _, err := Build([]Entry{e}); err == nil {
			t.Errorf("Build accepted %v", e)
		}
	}
}

func TestTooManyEntries(t *testing.T) {
	one, _ := Build([]Entry{{"100644", "f", id20}})
	raw := bytes.Repeat(one, MaxEntries+1)
	if _, err := Parse(raw, 20); err == nil {
		t.Fatal("more than MaxEntries accepted")
	}
}

func TestCanonicalMode(t *testing.T) {
	for in, want := range map[string]string{"040000": "40000", "100644": "100644", "120000": "120000"} {
		if got, ok := CanonicalMode(in); !ok || got != want {
			t.Errorf("%s -> %s %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "000000", "100666", "40000x"} {
		if _, ok := CanonicalMode(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func FuzzParse(f *testing.F) {
	ok, _ := Build([]Entry{{"100644", "a", id20}, {"40000", "d", id20}})
	f.Add(ok, 20)
	f.Add([]byte("100644 f\x00"), 20)
	f.Add([]byte{}, 32)
	f.Fuzz(func(t *testing.T, raw []byte, idLen int) {
		es, err := Parse(raw, idLen)
		if err != nil {
			return
		}
		// Everything Parse accepts rebuilds to exactly the same bytes.
		back, err := Build(es)
		if err != nil || !bytes.Equal(back, raw) {
			t.Fatalf("Parse accepted %q but Build gives %q (%v)", raw, back, err)
		}
	})
}
