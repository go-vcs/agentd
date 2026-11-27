// Package gittree parses and builds the raw content of a git tree object:
// repeated "<octal mode> <name>\0<binary id>" records.
//
// Parse is a parser of untrusted bytes (a response from the upstream) and is
// fuzzed with a committed corpus.
package gittree

import (
	"encoding/hex"
	"errors"
)

// Bounds on one tree.
const (
	MaxEntries = 100000
	MaxName    = 4096
)

// Entry is one record of a tree.
type Entry struct {
	Mode string `json:"mode"` // as git writes it: "100644", "40000", "120000", "160000"
	Name string `json:"name"`
	ID   string `json:"id"` // lowercase hex, full
}

// Kind is "tree", "commit" (a submodule) or "blob" for the entry's mode.
func (e Entry) Kind() string {
	switch e.Mode {
	case "40000":
		return "tree"
	case "160000":
		return "commit"
	}
	return "blob"
}

// ErrMalformed is returned for bytes that are not a well-formed tree.
var ErrMalformed = errors.New("malformed tree object")

// Parse reads a raw tree whose ids are idLen bytes long (20 for SHA-1, 32 for
// SHA-256). It refuses a mode that is not canonical, an empty name, a name with
// a slash or a NUL, a "." or ".." name, and more than MaxEntries records.
func Parse(raw []byte, idLen int) ([]Entry, error) {
	if idLen != 20 && idLen != 32 {
		return nil, ErrMalformed
	}
	var out []Entry
	for len(raw) > 0 {
		if len(out) >= MaxEntries {
			return nil, ErrMalformed
		}
		sp := indexByte(raw, ' ')
		if sp < 1 || sp > 6 || !canonicalMode(string(raw[:sp])) {
			return nil, ErrMalformed
		}
		mode := string(raw[:sp])
		raw = raw[sp+1:]
		nul := indexByte(raw, 0)
		if nul < 1 || nul > MaxName {
			return nil, ErrMalformed
		}
		name := string(raw[:nul])
		if !validName(name) {
			return nil, ErrMalformed
		}
		raw = raw[nul+1:]
		if len(raw) < idLen {
			return nil, ErrMalformed
		}
		out = append(out, Entry{Mode: mode, Name: name, ID: hex.EncodeToString(raw[:idLen])})
		raw = raw[idLen:]
	}
	return out, nil
}

// Build is the inverse of Parse. It returns an error for an entry Parse would
// refuse, so that Build(Parse(x)) == x for every x Parse accepts.
func Build(entries []Entry) ([]byte, error) {
	var b []byte
	for _, e := range entries {
		id, err := hex.DecodeString(e.ID)
		if err != nil || (len(id) != 20 && len(id) != 32) || !canonicalMode(e.Mode) || !validName(e.Name) {
			return nil, ErrMalformed
		}
		b = append(b, e.Mode...)
		b = append(b, ' ')
		b = append(b, e.Name...)
		b = append(b, 0)
		b = append(b, id...)
	}
	return b, nil
}

// CanonicalMode converts the zero-padded mode of GitHub's JSON ("040000") to
// git's raw form ("40000"); it reports false for an unknown mode.
func CanonicalMode(s string) (string, bool) {
	if len(s) == 6 && s[0] == '0' && s != "000000" {
		s = s[1:]
	}
	return s, canonicalMode(s)
}

func canonicalMode(m string) bool {
	switch m {
	case "100644", "100755", "40000", "120000", "160000":
		return true
	}
	return false
}

func validName(n string) bool {
	if n == "" || n == "." || n == ".." || len(n) > MaxName {
		return false
	}
	for i := 0; i < len(n); i++ {
		if n[i] == '/' || n[i] == 0 {
			return false
		}
	}
	return true
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
