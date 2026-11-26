package oid

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	full := strings.Repeat("a1", 20)
	full64 := strings.Repeat("0f", 32)
	for _, tc := range []struct {
		name string
		in   string
		want error
	}{
		{"sha1", full, nil},
		{"sha256", full64, nil},
		{"seven char prefix", full[:7], ErrAbbreviated},
		{"39 digits", full[:39], ErrAbbreviated},
		{"41 digits", full + "a", ErrMalformed},
		{"63 digits", full64[:63], ErrMalformed},
		{"empty", "", ErrMalformed},
		{"uppercase", strings.ToUpper(full), ErrMalformed},
		{"non hex 40", strings.Repeat("g", 40), ErrMalformed},
		{"short non hex", "main", ErrMalformed},
		{"newline", full + "\n", ErrMalformed},
		{"ref name", "refs/heads/main", ErrMalformed},
	} {
		got, err := Parse(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if err == nil && got != tc.in {
			t.Errorf("%s: got %q", tc.name, got)
		}
		if err != nil && got != "" {
			t.Errorf("%s: id returned with an error: %q", tc.name, got)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"", strings.Repeat("a", 40), strings.Repeat("a", 7), strings.Repeat("F", 40), "é", strings.Repeat("0", 64)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Parse(s)
		if err != nil {
			return
		}
		if got != s || (len(s) != 40 && len(s) != 64) {
			t.Fatalf("accepted %q", s)
		}
	})
}
