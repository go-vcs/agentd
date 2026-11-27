package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/wire"
)

// fakeDaemon serves hello and the two object routes on a unix socket and
// returns its path. mode picks what the object routes answer.
func fakeDaemon(t *testing.T, mode string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hello", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wire.HelloReply{Session: "s1", Label: "x"})
	})
	refuse := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(wire.ErrorBody{Error: aerr.New(aerr.ObjectMismatch, "refused")})
	}
	mux.HandleFunc("/v1/read-blob", func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "refuse":
			refuse(w)
		case "badbase64":
			json.NewEncoder(w).Encode(wire.BlobReply{Content: "!!not base64!!"})
		default:
			json.NewEncoder(w).Encode(wire.BlobReply{Content: base64.StdEncoding.EncodeToString([]byte("hello\n")), Size: 6})
		}
	})
	mux.HandleFunc("/v1/read-tree", func(w http.ResponseWriter, r *http.Request) {
		if mode == "refuse" {
			refuse(w)
			return
		}
		json.NewEncoder(w).Encode(wire.TreeReply{Entries: []gittree.Entry{{Mode: "100644", Name: "a", ID: strings.Repeat("ab", 20)}}})
	})
	mux.HandleFunc("/v1/materialize", func(w http.ResponseWriter, r *http.Request) {
		if mode == "refuse" {
			refuse(w)
			return
		}
		var req wire.MaterializeReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(wire.MaterializeReply{Commit: req.Commit, Dir: "/root/" + req.Dir, Files: len(req.Paths)})
	})
	// /v1/events answers a script: PENDING, a quiet timeout, then RED. Each step
	// is only given to a caller that sends the cursor of the step before.
	var calls int
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if mode == "refuse" {
			refuse(w)
			return
		}
		var req wire.EventsReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
		calls++
		var rep wire.EventsReply
		switch {
		case calls == 1 && req.Since == "":
			rep = wire.EventsReply{Cursor: a}
			rep.Commit, rep.State = req.Commit, "PENDING"
		case calls == 2 && req.Since == a:
			rep = wire.EventsReply{Cursor: a, TimedOut: true}
			rep.Commit, rep.State = req.Commit, "PENDING"
		case calls == 3 && req.Since == a:
			rep = wire.EventsReply{Cursor: b, Final: true}
			rep.Commit, rep.State = req.Commit, "RED"
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(wire.ErrorBody{Error: aerr.New(aerr.Malformed, "unexpected call")})
			return
		}
		json.NewEncoder(w).Encode(rep)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func runAgent(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := AgentCmd()
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs(args)
	survives(t, strings.Join(args, " "), func() { err = c.Execute() })
	return out.String(), errb.String(), err
}

var id40 = strings.Repeat("a", 40)

func TestBlobAndTreeWriteTheirAnswer(t *testing.T) {
	sock := fakeDaemon(t, "ok")
	out, _, err := runAgent(t, "--label", "x", "--socket", sock, "blob", "o/r", id40)
	if err != nil || out != "hello\n" {
		t.Fatalf("blob: %q %v", out, err)
	}
	out, _, err = runAgent(t, "--label", "x", "--socket", sock, "tree", "o/r", id40)
	if err != nil || !strings.Contains(out, `"name":"a"`) || !strings.Contains(out, `"mode":"100644"`) {
		t.Fatalf("tree: %q %v", out, err)
	}
}

func TestBlobAndTreeRefuseWithoutALabelOrADaemon(t *testing.T) {
	for _, cmd := range []string{"blob", "tree"} {
		_, _, err := runAgent(t, "--socket", "/nonexistent/s", cmd, "o/r", id40)
		if err == nil || !strings.Contains(err.Error(), "--label") {
			t.Errorf("%s without a label: %v", cmd, err)
		}
		_, errb, err := runAgent(t, "--label", "x", "--socket", "/nonexistent/s", cmd, "o/r", id40)
		ee, ok := err.(exitError)
		if !ok || ee.code != ExitError || !strings.Contains(errb, `"reason":"UNAVAILABLE"`) {
			t.Errorf("%s unreachable: %v %q", cmd, err, errb)
		}
	}
}

func TestBlobAndTreeReportADaemonRefusal(t *testing.T) {
	sock := fakeDaemon(t, "refuse")
	for _, cmd := range []string{"blob", "tree"} {
		out, errb, err := runAgent(t, "--label", "x", "--socket", sock, cmd, "o/r", id40)
		ee, ok := err.(exitError)
		if !ok || ee.code != ExitError || out != "" || !strings.Contains(errb, `"reason":"OBJECT_MISMATCH"`) {
			t.Errorf("%s: %v out %q err %q", cmd, err, out, errb)
		}
	}
}

func TestBlobWithUndecodableContentIsInternalAndWritesNothing(t *testing.T) {
	sock := fakeDaemon(t, "badbase64")
	out, errb, err := runAgent(t, "--label", "x", "--socket", sock, "blob", "o/r", id40)
	ee, ok := err.(exitError)
	if !ok || ee.code != ExitError || out != "" || !strings.Contains(errb, `"reason":"INTERNAL"`) {
		t.Fatalf("%v out %q err %q", err, out, errb)
	}
}
