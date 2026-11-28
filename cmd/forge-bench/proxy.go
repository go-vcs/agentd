package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

// proxy is a loopback reverse proxy in front of the GitHub API that counts what
// the daemon asks upstream: requests, and response body bytes as the server sent
// them (compression is left as the daemon negotiated it).
type proxy struct {
	srv   *http.Server
	url   string
	reqs  atomic.Int64
	bytes atomic.Int64
}

type countBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

type countTransport struct {
	rt http.RoundTripper
	p  *proxy
}

func (t countTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.p.reqs.Add(1)
	resp, err := t.rt.RoundTrip(r)
	if err == nil {
		resp.Body = countBody{resp.Body, &t.p.bytes}
	}
	return resp, err
}

// listenLoopback is a variable so that a test can make the listen fail.
var listenLoopback = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// startProxy listens on a loopback port and forwards to target.
func startProxy(target string) (*proxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	ln, err := listenLoopback()
	if err != nil {
		return nil, err
	}
	p := &proxy{url: "http://" + ln.Addr().String()}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	rp := &httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.SetURL(u) },
		Transport: countTransport{tr, p},
	}
	p.srv = &http.Server{Handler: rp, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *proxy) reset() { p.reqs.Store(0); p.bytes.Store(0) }

func (p *proxy) snapshot() (int, int64) { return int(p.reqs.Load()), p.bytes.Load() }

func (p *proxy) close() { _ = p.srv.Shutdown(context.Background()) }
