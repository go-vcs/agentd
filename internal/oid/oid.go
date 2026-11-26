// Package oid validates object ids.
//
// The agent-VCS design (forge docs/agent-vcs.md, "Data model") accepts full
// ids only: 40 lowercase hexadecimal digits for SHA-1, 64 for SHA-256. An
// abbreviated id is refused and never expanded, because a full id rebuilt from
// the 7 characters a tool printed answers "no runs" for a commit that has some.
package oid

import "errors"

// ErrAbbreviated is returned for a hexadecimal id shorter than a full one.
var ErrAbbreviated = errors.New("abbreviated object id: a full 40 or 64 digit id is required")

// ErrMalformed is returned for anything that is not a lowercase hex id.
var ErrMalformed = errors.New("malformed object id")

// Parse returns s when it is a full object id and an error otherwise.
func Parse(s string) (string, error) {
	hexOnly := s != ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			hexOnly = false
			break
		}
	}
	if !hexOnly {
		return "", ErrMalformed
	}
	switch len(s) {
	case 40, 64:
		return s, nil
	}
	if len(s) < 40 {
		return "", ErrAbbreviated
	}
	return "", ErrMalformed
}
