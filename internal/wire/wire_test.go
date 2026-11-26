package wire

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
)

func TestValidators(t *testing.T) {
	good := strings.Repeat("a1", 16)
	for s, want := range map[string]bool{good: true, strings.ToUpper(good): false, good[:31]: false, good + "0": false, "": false, strings.Repeat("g", 32): false} {
		if ValidRequestID(s) != want {
			t.Errorf("request id %q", s)
		}
	}
	for s, want := range map[string]bool{"s-" + good: true, good: false, "s-": false, "s-" + good + "\n": false} {
		if ValidSession(s) != want {
			t.Errorf("session %q", s)
		}
	}
	for s, want := range map[string]bool{"a": true, "héllo": true, strings.Repeat("x", 64): true, strings.Repeat("x", 65): false, "": false,
		"a\tb": false, "a\x7fb": false, "a\u0085b": false, "\xff": false, "a\x1fb": false} {
		if ValidLabel(s) != want {
			t.Errorf("label %q", s)
		}
	}
}

func TestDecode(t *testing.T) {
	var h Hello
	if err := Decode(strings.NewReader(`{"label":"a"}`), 100, &h); err != nil || h.Label != "a" {
		t.Fatal(err)
	}
	if err := Decode(strings.NewReader(`{"label":"a"}  `), 100, &h); err != nil {
		t.Errorf("trailing whitespace: %v", err)
	}
	for _, in := range []string{`{"label":"a"} x`, `{"label":"a"}{}`, `{"x":1}`, ``, `{`} {
		if err := Decode(strings.NewReader(in), 100, &h); aerr.As(err).Reason != aerr.Malformed {
			t.Errorf("%q: %v", in, err)
		}
	}
	err := Decode(strings.NewReader(`{"label":"`+strings.Repeat("a", 100)+`"}`), 20, &h)
	if aerr.As(err).Reason != aerr.BoundExceeded {
		t.Errorf("bound: %v", err)
	}
	if err := Decode(strings.NewReader(strings.Repeat(" ", 20)+"{}"), 20, &h); aerr.As(err).Reason != aerr.BoundExceeded {
		t.Errorf("exact bound+: %v", err)
	}
	if err := Decode(strings.NewReader(`{"label":"a"}`), int64(len(`{"label":"a"}`)), &h); err != nil {
		t.Errorf("exactly at the bound: %v", err)
	}
	if aerr.As(errors.New("x")).Reason != aerr.Internal {
		t.Error("foreign error not Internal")
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range []string{``, `{}`, `{"label":"a"}`, `{"session":"s","repo":"o/r","pr":1}`, `[`, `{"label":"a"}{"label":"b"}`, "\xff"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var h Hello
		var m MergeReq
		var w WaitReq
		var v VerdictReq
		_ = Decode(strings.NewReader(string(b)), 1<<10, &h)
		_ = Decode(strings.NewReader(string(b)), 1<<10, &m)
		_ = Decode(strings.NewReader(string(b)), 1<<10, &w)
		_ = Decode(strings.NewReader(string(b)), 1<<10, &v)
		_ = ValidLabel(string(b))
		_ = ValidRequestID(string(b))
		_ = ValidSession(string(b))
	})
}
