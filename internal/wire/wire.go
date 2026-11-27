// Package wire holds the local protocol's messages and the one strict decoder
// every request goes through.
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/verdict"
)

// Hello opens (or reuses) the session of a local label.
type Hello struct {
	Label string `json:"label"`
}

// HelloReply names the session every later call carries.
type HelloReply struct {
	Session string `json:"session"`
	Label   string `json:"label"`
}

// VerdictReq asks for the gate verdict of one full commit.
type VerdictReq struct {
	Session string `json:"session"`
	Repo    string `json:"repo"`
	Commit  string `json:"commit"`
}

// WaitReq blocks until the verdict of Commit is not PENDING.
type WaitReq struct {
	Session    string `json:"session"`
	Repo       string `json:"repo"`
	Commit     string `json:"commit"`
	TimeoutSec int    `json:"timeout_sec"`
}

// VerdictReply is a verdict; TimedOut is set when the wait ended while the
// commit was still pending.
type VerdictReply struct {
	verdict.Verdict
	TimedOut bool `json:"timed_out,omitempty"`
}

// EventsReq asks for the next observation of a commit's verdict after Since
// (empty: any), waiting at most TimeoutSec.
type EventsReq struct {
	Session    string `json:"session"`
	Repo       string `json:"repo"`
	Commit     string `json:"commit"`
	Since      string `json:"since,omitempty"`
	TimeoutSec int    `json:"timeout_sec"`
}

// EventsReply is one observation. Cursor goes back as Since; Final means the
// verdict is not PENDING and the stream is over; TimedOut means nothing changed
// in time and Verdict is the latest observation (Cursor is then Since).
type EventsReply struct {
	verdict.Verdict
	Cursor   string `json:"cursor"`
	Final    bool   `json:"final,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// MergeReq merges one pull request, only if Commit is its head and is green.
type MergeReq struct {
	Session   string `json:"session"`
	RequestID string `json:"request_id"`
	Repo      string `json:"repo"`
	PR        int    `json:"pr"`
	Commit    string `json:"commit"`
	Method    string `json:"method"`
}

// MergeReply names both commits: the one judged and the one that landed.
type MergeReply struct {
	Repo        string `json:"repo"`
	PR          int    `json:"pr"`
	Commit      string `json:"commit"`
	MergeCommit string `json:"merge_commit"`
}

// ObjectReq reads one blob or tree by its full id.
type ObjectReq struct {
	Session string `json:"session"`
	Repo    string `json:"repo"`
	ID      string `json:"id"`
}

// BlobReply carries a blob's bytes (standard base64) once the daemon has
// recomputed the id from them. Cached says the answer came from the cache.
type BlobReply struct {
	ID      string `json:"id"`
	Size    int    `json:"size"`
	Content string `json:"content"`
	Cached  bool   `json:"cached"`
}

// TreeReply carries a tree's entries once the daemon has recomputed the id from
// the tree's raw bytes.
type TreeReply struct {
	ID      string          `json:"id"`
	Entries []gittree.Entry `json:"entries"`
	Cached  bool            `json:"cached"`
}

// MaterializeReq asks for a sparse checkout of Commit (restricted to Paths;
// empty: everything) into the new directory Dir, relative to the daemon's
// materialize root.
type MaterializeReq struct {
	Session string   `json:"session"`
	Repo    string   `json:"repo"`
	Commit  string   `json:"commit"`
	Paths   []string `json:"paths,omitempty"`
	Dir     string   `json:"dir"`
}

// MaterializeReply names the commit and tree written and the absolute path.
type MaterializeReply struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	Dir    string `json:"dir"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// ErrorBody is the body of every refusal.
type ErrorBody struct {
	Error *aerr.Error `json:"error"`
}

// Decode reads exactly one JSON object of at most max bytes into v, refusing
// unknown fields and trailing data.
func Decode(r io.Reader, max int64, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return aerr.New(aerr.BoundExceeded, "request larger than the bound")
		}
		return aerr.New(aerr.Malformed, "request unreadable")
	}
	if int64(len(b)) > max {
		return aerr.New(aerr.BoundExceeded, "request larger than the bound")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return aerr.New(aerr.Malformed, "request is not the expected JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return aerr.New(aerr.Malformed, "trailing data after the request")
	}
	return nil
}

var (
	requestIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sessionRE   = regexp.MustCompile(`^s-[0-9a-f]{32}$`)
)

// ValidRequestID: 16 random bytes in lowercase hex, client-chosen.
func ValidRequestID(s string) bool { return requestIDRE.MatchString(s) }

// ValidCursor reports whether s has the shape of an events cursor (32 lowercase
// hex digits, the same shape as a request id).
func ValidCursor(s string) bool { return requestIDRE.MatchString(s) }

// ValidSession reports whether s has the shape of a session id.
func ValidSession(s string) bool { return sessionRE.MatchString(s) }

// MaxLabel is the longest label, in bytes.
const MaxLabel = 64

// ValidLabel: 1..64 bytes of printable UTF-8 without control characters.
func ValidLabel(s string) bool {
	if s == "" || len(s) > MaxLabel || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}
