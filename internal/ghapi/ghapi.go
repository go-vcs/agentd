// Package ghapi is the GitHub REST backend of forge-agentd.
//
// The token is read from a file of mode 0600 and used only as a bearer header:
// it is never in argv, the environment or a URL, and no error carries it.
package ghapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/oid"
	"github.com/go-vcs/agentd/internal/verdict"
)

// Bounds on what the upstream may make us hold.
const (
	MaxResponse = 8 << 20 // bytes of one response
	MaxPages    = 10      // pages of 100 for one listing
	maxToken    = 1 << 10
)

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// ValidRepo reports whether s is an owner/name that cannot escape its path.
func ValidRepo(s string) bool {
	if !repoRE.MatchString(s) {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "." || p == ".." {
			return false
		}
	}
	return true
}

// Client talks to the GitHub REST API.
type Client struct {
	base        *url.URL
	token       string
	hc          *http.Client
	maxResponse int64
	maxPages    int
}

// ReadToken reads a token from path. The file must not be readable by group or
// others (unix), and the value is never echoed.
func ReadToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", errors.Unwrap(err))
	}
	defer f.Close()
	st, _ := f.Stat() // fstat on a descriptor just opened does not fail
	if !st.Mode().IsRegular() {
		return "", errors.New("token file: not a regular file")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file: mode %04o lets others read it; chmod 600", st.Mode().Perm())
	}
	// A read error on a regular file just opened (EIO) leaves a short or empty
	// buffer, which the checks below refuse.
	b, _ := io.ReadAll(io.LimitReader(f, maxToken+1))
	if len(b) > maxToken {
		return "", errors.New("token file: larger than 1 KiB")
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return "", errors.New("token file: empty or not a single token")
	}
	return tok, nil
}

// New builds a client. apiURL must be https, or http on a loopback address
// (tests): a token is never sent in clear over a network.
func New(apiURL, token string) (*Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("api url: not a URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		host, _, _ := net.SplitHostPort(u.Host)
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("api url: http is allowed on loopback only")
		}
	default:
		return nil, errors.New("api url: scheme must be https")
	}
	if token == "" {
		return nil, errors.New("no token")
	}
	return &Client{
		base: u, token: token, maxResponse: MaxResponse, maxPages: MaxPages,
		hc: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect would carry the Authorization header to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawQuery = q.Encode()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body) // body is a map[string]string: it cannot fail
		rd = bytes.NewReader(b)
	}
	// method is a constant, u was validated by New and its path built from
	// checked repo names: the request cannot fail to build.
	req, _ := http.NewRequestWithContext(ctx, method, u.String(), rd)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "forge-agentd")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return aerr.New(aerr.Unavailable, "upstream unreachable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return aerr.New(aerr.Unavailable, "upstream response cut")
	}
	if int64(len(b)) > c.maxResponse {
		return aerr.New(aerr.BoundExceeded, "upstream response larger than "+strconv.FormatInt(c.maxResponse, 10)+" bytes")
	}
	if resp.StatusCode/100 != 2 {
		return statusError(resp, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return aerr.New(aerr.Unavailable, "upstream answer unreadable")
	}
	return nil
}

func statusError(resp *http.Response, body []byte) error {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0",
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != "":
		e := aerr.New(aerr.RateLimited, "GitHub rate limit")
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 && n < 3600 {
			e.RetryAfterSec = n
		}
		return e
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return aerr.New(aerr.NotPermitted, "GitHub refused the daemon's credential")
	case resp.StatusCode == http.StatusNotFound:
		return aerr.New(aerr.NotFound, "not found (or not visible to the daemon's credential)")
	case resp.StatusCode == http.StatusConflict:
		return aerr.New(aerr.RefMoved, "head commit does not match")
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return aerr.New(aerr.Conflict, "pull request is not mergeable")
	case resp.StatusCode >= 500:
		return aerr.New(aerr.Unavailable, "upstream "+strconv.Itoa(resp.StatusCode))
	}
	return aerr.New(aerr.Malformed, "upstream refused the request: "+strconv.Itoa(resp.StatusCode))
}

type runsReply struct {
	Total int           `json:"total_count"`
	Runs  []verdict.Run `json:"workflow_runs"`
}

type checksReply struct {
	Total  int             `json:"total_count"`
	Checks []verdict.Check `json:"check_runs"`
}

type jobsReply struct {
	Total *int `json:"total_count"`
}

// DecodeRuns reads an actions/runs page.
func DecodeRuns(b []byte) ([]verdict.Run, int, error) {
	var r runsReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, 0, err
	}
	for i := range r.Runs {
		r.Runs[i].Jobs = verdict.JobsUnknown
	}
	return r.Runs, r.Total, nil
}

// DecodeChecks reads a commits/{sha}/check-runs page.
func DecodeChecks(b []byte) ([]verdict.Check, int, error) {
	var r checksReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, 0, err
	}
	return r.Checks, r.Total, nil
}

// DecodeJobCount reads the job count of a run. A reply without the field is an
// error, never a count of zero: absence is not a measurement.
func DecodeJobCount(b []byte) (int, error) {
	var r jobsReply
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	if r.Total == nil || *r.Total < 0 {
		return 0, errors.New("no total_count")
	}
	return *r.Total, nil
}

func (c *Client) runs(ctx context.Context, repo, sha string) ([]verdict.Run, error) {
	var all []verdict.Run
	for page := 1; ; page++ {
		var raw json.RawMessage
		q := url.Values{"head_sha": {sha}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runs", q, nil, &raw); err != nil {
			return nil, err
		}
		runs, total, err := DecodeRuns(raw)
		if err != nil {
			return nil, aerr.New(aerr.Unavailable, "upstream answer unreadable")
		}
		all = append(all, runs...)
		if len(all) >= total || len(runs) == 0 {
			return all, nil
		}
		if page >= c.maxPages {
			return nil, aerr.New(aerr.BoundExceeded, "more than "+strconv.Itoa(c.maxPages*100)+" workflow runs for one commit")
		}
	}
}

func (c *Client) checks(ctx context.Context, repo, sha string) ([]verdict.Check, error) {
	var all []verdict.Check
	for page := 1; ; page++ {
		var raw json.RawMessage
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/check-runs", q, nil, &raw); err != nil {
			return nil, err
		}
		checks, total, err := DecodeChecks(raw)
		if err != nil {
			return nil, aerr.New(aerr.Unavailable, "upstream answer unreadable")
		}
		all = append(all, checks...)
		if len(all) >= total || len(checks) == 0 {
			return all, nil
		}
		if page >= c.maxPages {
			return nil, aerr.New(aerr.BoundExceeded, "more than "+strconv.Itoa(c.maxPages*100)+" check runs for one commit")
		}
	}
}

// Verdict reads every run and check of the full commit sha and judges them.
func (c *Client) Verdict(ctx context.Context, repo, sha string) (verdict.Verdict, error) {
	if !ValidRepo(repo) {
		return verdict.Verdict{}, aerr.New(aerr.Malformed, "repo must be owner/name")
	}
	// ⛔ Full ids only: head_sha answers [] for an abbreviation, and an empty
	// list is indistinguishable from "this commit has no runs".
	if _, err := oid.Parse(sha); err != nil {
		return verdict.Verdict{}, idError(err)
	}
	runs, err := c.runs(ctx, repo, sha)
	if err != nil {
		return verdict.Verdict{}, err
	}
	checks, err := c.checks(ctx, repo, sha)
	if err != nil {
		return verdict.Verdict{}, err
	}
	runs = verdict.Latest(runs)
	for i := range runs {
		r := &runs[i]
		// Only a run that claims a pass needs its jobs counted: any other
		// conclusion is already not green.
		if !strings.EqualFold(r.Status, "completed") || !countable(r.Conclusion) {
			continue
		}
		var raw json.RawMessage
		q := url.Values{"per_page": {"1"}}
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runs/"+strconv.FormatInt(r.ID, 10)+"/jobs", q, nil, &raw); err != nil {
			return verdict.Verdict{}, err
		}
		n, err := DecodeJobCount(raw)
		if err != nil {
			return verdict.Verdict{}, aerr.New(aerr.Unavailable, "upstream gave no job count")
		}
		r.Jobs = n
	}
	v, err := verdict.Judge(sha, runs, checks)
	if err != nil {
		// An observation about another commit: refused, never merged in.
		return verdict.Verdict{}, aerr.New(aerr.Unavailable, "upstream answered about another commit")
	}
	return v, nil
}

func countable(conclusion string) bool {
	switch strings.ToLower(conclusion) {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

func idError(err error) *aerr.Error {
	if errors.Is(err, oid.ErrAbbreviated) {
		return aerr.New(aerr.AbbreviatedID, "a full object id is required")
	}
	return aerr.New(aerr.Malformed, "malformed object id")
}

// PR is what the merge gate needs of a pull request.
type PR struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// PRHead reads the pull request's current head commit.
func (c *Client) PRHead(ctx context.Context, repo string, number int) (PR, error) {
	if !ValidRepo(repo) || number <= 0 {
		return PR{}, aerr.New(aerr.Malformed, "repo must be owner/name and pr positive")
	}
	var p PR
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, nil, &p); err != nil {
		return PR{}, err
	}
	if _, err := oid.Parse(p.Head.SHA); err != nil {
		return PR{}, aerr.New(aerr.Unavailable, "upstream gave no full head commit")
	}
	return p, nil
}

// MergePR merges with GitHub's sha precondition: it answers 409 when the head
// is no longer sha, which the daemon reports as REF_MOVED.
func (c *Client) MergePR(ctx context.Context, repo string, number int, sha, method string) (string, error) {
	if !ValidRepo(repo) || number <= 0 {
		return "", aerr.New(aerr.Malformed, "repo must be owner/name and pr positive")
	}
	if _, err := oid.Parse(sha); err != nil {
		return "", idError(err)
	}
	switch method {
	case "merge", "squash", "rebase":
	default:
		return "", aerr.New(aerr.Malformed, "method must be merge, squash or rebase")
	}
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	body := map[string]string{"sha": sha, "merge_method": method}
	if err := c.do(ctx, http.MethodPut, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/merge", nil, body, &out); err != nil {
		return "", err
	}
	if !out.Merged {
		return "", aerr.New(aerr.Conflict, "upstream did not merge")
	}
	return out.SHA, nil
}

// Head returns the head commit of an OPEN pull request; a merged or closed one
// answers CHANGE_CLOSED, since nothing can be merged into it.
func (c *Client) Head(ctx context.Context, repo string, number int) (string, error) {
	p, err := c.PRHead(ctx, repo, number)
	if err != nil {
		return "", err
	}
	if p.State != "open" || p.Merged {
		return "", aerr.New(aerr.ChangeClosed, "pull request is not open")
	}
	return p.Head.SHA, nil
}

// Merge is MergePR under the name the daemon's Backend uses.
func (c *Client) Merge(ctx context.Context, repo string, number int, sha, method string) (string, error) {
	return c.MergePR(ctx, repo, number, sha, method)
}

// MaxObject is the largest blob the client decodes.
const MaxObject = 2 << 20

type blobReply struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
}

// DecodeBlob reads a git/blobs answer into the blob's bytes. The size is
// checked before anything is decoded, and the decoded length must equal it.
func DecodeBlob(b []byte, max int64) ([]byte, error) {
	var r blobReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Size < 0 || r.Size > max {
		return nil, errBlobTooLarge
	}
	switch r.Encoding {
	case "base64":
		s := strings.ReplaceAll(r.Content, "\n", "")
		if int64(len(s)) > (max+2)/3*4 {
			return nil, errBlobTooLarge
		}
		d, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		if int64(len(d)) != r.Size {
			return nil, errors.New("blob size does not match its content")
		}
		return d, nil
	case "utf-8":
		if int64(len(r.Content)) != r.Size {
			return nil, errors.New("blob size does not match its content")
		}
		return []byte(r.Content), nil
	}
	return nil, errors.New("unknown blob encoding")
}

var errBlobTooLarge = errors.New("blob larger than the bound")

type treeReply struct {
	Truncated bool `json:"truncated"`
	Tree      []struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

// DecodeTree reads a git/trees answer (not recursive) into tree entries. A
// truncated listing is refused: the tree could not be rebuilt and verified.
func DecodeTree(b []byte) ([]gittree.Entry, error) {
	var r treeReply
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Truncated || len(r.Tree) > gittree.MaxEntries {
		return nil, errors.New("tree listing truncated or too large")
	}
	out := make([]gittree.Entry, 0, len(r.Tree))
	for _, e := range r.Tree {
		m, ok := gittree.CanonicalMode(e.Mode)
		if !ok {
			return nil, errors.New("unknown tree entry mode")
		}
		out = append(out, gittree.Entry{Mode: m, Name: e.Path, ID: e.SHA})
	}
	return out, nil
}

// Object returns the raw content (what git hashes after its header) of the
// blob or tree id. It is NOT verified here: the daemon's cache recomputes the
// id before it keeps or returns anything.
func (c *Client) Object(ctx context.Context, repo, kind, id string) ([]byte, error) {
	if !ValidRepo(repo) {
		return nil, aerr.New(aerr.Malformed, "repo must be owner/name")
	}
	if _, err := oid.Parse(id); err != nil {
		return nil, idError(err)
	}
	var raw json.RawMessage
	switch kind {
	case "blob":
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/blobs/"+id, nil, nil, &raw); err != nil {
			return nil, err
		}
		d, err := DecodeBlob(raw, MaxObject)
		if errors.Is(err, errBlobTooLarge) {
			return nil, aerr.New(aerr.BoundExceeded, "blob larger than "+strconv.Itoa(MaxObject)+" bytes")
		}
		if err != nil {
			return nil, aerr.New(aerr.Unavailable, "upstream answer unreadable")
		}
		return d, nil
	case "tree":
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/trees/"+id, nil, nil, &raw); err != nil {
			return nil, err
		}
		es, err := DecodeTree(raw)
		if err != nil {
			return nil, aerr.New(aerr.Unavailable, "upstream answer unreadable")
		}
		d, err := gittree.Build(es)
		if err != nil {
			return nil, aerr.New(aerr.Unavailable, "upstream answer unreadable")
		}
		return d, nil
	}
	return nil, aerr.New(aerr.Malformed, "kind must be blob or tree")
}
