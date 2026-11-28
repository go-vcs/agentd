package ghapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxParents bounds the parents one commit may list (an octopus merge).
const maxParents = 256

type commitIdent struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitReply struct {
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Author       commitIdent `json:"author"`
	Committer    commitIdent `json:"committer"`
	Message      string      `json:"message"`
	Verification struct {
		Signature *string `json:"signature"`
		Payload   *string `json:"payload"`
	} `json:"verification"`
}

// DecodeCommit rebuilds the raw commit object (what git hashes after its
// header) from a git/commits answer. It is NOT trusted to be right: the daemon's
// cache recomputes the id from these bytes, so a commit that cannot be rebuilt
// exactly (an extra header such as mergetag or encoding) is refused there as an
// id mismatch, never guessed at.
//
// A signed commit is rebuilt from verification.payload (the commit as signed)
// plus its gpgsig header; any other commit from its fields. The answer does not
// say whether the message ended with a newline (it did not for the signed golden
// commit in testdata, it did for the unsigned one) and drops the offset of an
// unsigned commit's dates, so only a signed commit is rebuilt exactly. The
// daemon reads commits from the git endpoint (fetchCommit) and keeps this
// rebuild for an upstream that has none (--git-url unset on a non-GitHub API).
func DecodeCommit(b []byte) ([]byte, error) {
	var r commitReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	var raw []byte
	if p := r.Verification.Payload; p != nil && *p != "" {
		payload := []byte(*p)
		if s := r.Verification.Signature; s != nil && *s != "" {
			i := bytes.Index(payload, []byte("\n\n"))
			if i < 0 {
				return nil, errors.New("commit payload has no message separator")
			}
			// git writes each line of the signature after "gpgsig " or a space, and a
			// signature that ends with a newline ends with an empty continuation line.
			raw = append(raw, payload[:i+1]...)
			raw = append(raw, "gpgsig "+strings.ReplaceAll(*s, "\n", "\n ")+"\n"...)
			raw = append(raw, payload[i+1:]...)
		} else {
			raw = payload
		}
	} else {
		var err error
		if raw, err = rebuildCommit(&r); err != nil {
			return nil, err
		}
	}
	if len(raw) > MaxObject {
		return nil, errBlobTooLarge
	}
	return raw, nil
}

func rebuildCommit(r *commitReply) ([]byte, error) {
	if len(r.Parents) > maxParents {
		return nil, errors.New("commit lists too many parents")
	}
	if !hexID(r.Tree.SHA) {
		return nil, errors.New("commit tree id is not a full id")
	}
	var b strings.Builder
	b.WriteString("tree " + r.Tree.SHA + "\n")
	for _, p := range r.Parents {
		if !hexID(p.SHA) {
			return nil, errors.New("commit parent id is not a full id")
		}
		b.WriteString("parent " + p.SHA + "\n")
	}
	a, err := identLine("author", r.Author)
	if err != nil {
		return nil, err
	}
	c, err := identLine("committer", r.Committer)
	if err != nil {
		return nil, err
	}
	b.WriteString(a + c + "\n" + r.Message)
	return []byte(b.String()), nil
}

func hexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// identLine is "<role> Name <email> <unix seconds> <+hhmm>\n".
func identLine(role string, id commitIdent) (string, error) {
	if strings.ContainsAny(id.Name, "\n<>\x00") || strings.ContainsAny(id.Email, "\n<>\x00") {
		return "", errors.New("commit identity holds a character git does not write")
	}
	t, err := time.Parse(time.RFC3339, id.Date)
	if err != nil {
		return "", errors.New("commit date is not RFC 3339")
	}
	// An RFC 3339 offset is whole hours and minutes, so off is a whole number of
	// minutes here.
	_, off := t.Zone()
	sign := byte('+')
	if off < 0 {
		sign, off = '-', -off
	}
	return fmt.Sprintf("%s %s <%s> %s %c%02d%02d\n", role, id.Name, id.Email, strconv.FormatInt(t.Unix(), 10), sign, off/3600, off%3600/60), nil
}
