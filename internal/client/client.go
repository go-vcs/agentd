// Package client is what an agent uses to call forge-agentd. It holds no
// credential: the daemon holds the only one.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/wire"
)

// MaxReply bounds what the client will read back.
const MaxReply = 4 << 20

// Client calls one daemon socket under one label.
type Client struct {
	hc      *http.Client
	base    string
	Label   string
	session string
}

// New returns a client for the daemon on socket.
func New(socket, label string) *Client {
	return &Client{
		base: "http://forge-agentd", Label: label,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		}},
	}
}

// NewRequestID returns 16 random bytes in hex, the idempotency key of a write.
func NewRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (c *Client) call(ctx context.Context, path string, req, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(hr)
	if err != nil {
		return aerr.New(aerr.Unavailable, "the daemon is not reachable on its socket")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxReply+1))
	if err != nil || len(body) > MaxReply {
		return aerr.New(aerr.Unavailable, "the daemon's reply is unreadable or too large")
	}
	if resp.StatusCode != http.StatusOK {
		var eb wire.ErrorBody
		if json.Unmarshal(body, &eb) != nil || eb.Error == nil {
			return aerr.New(aerr.Internal, "")
		}
		return eb.Error
	}
	return json.Unmarshal(body, out)
}

// Hello opens the session of the client's label (idempotent).
func (c *Client) Hello(ctx context.Context) error {
	var r wire.HelloReply
	if err := c.call(ctx, "/v1/hello", wire.Hello{Label: c.Label}, &r); err != nil {
		return err
	}
	c.session = r.Session
	return nil
}

// Verdict reads the gate verdict of one full commit.
func (c *Client) Verdict(ctx context.Context, repo, commit string) (wire.VerdictReply, error) {
	var r wire.VerdictReply
	err := c.call(ctx, "/v1/verdict", wire.VerdictReq{Session: c.session, Repo: repo, Commit: commit}, &r)
	return r, err
}

// Wait blocks (at the daemon) until the verdict is decided or timeout passes.
func (c *Client) Wait(ctx context.Context, repo, commit string, timeout time.Duration) (wire.VerdictReply, error) {
	var r wire.VerdictReply
	err := c.call(ctx, "/v1/wait-verdict", wire.WaitReq{Session: c.session, Repo: repo, Commit: commit, TimeoutSec: int(timeout / time.Second)}, &r)
	return r, err
}

// Merge merges a pull request iff commit is its head and is green.
func (c *Client) Merge(ctx context.Context, requestID, repo string, pr int, commit, method string) (wire.MergeReply, error) {
	var r wire.MergeReply
	err := c.call(ctx, "/v1/merge", wire.MergeReq{Session: c.session, RequestID: requestID, Repo: repo, PR: pr, Commit: commit, Method: method}, &r)
	return r, err
}
