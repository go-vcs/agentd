package objcache

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkVsGitCatFileBatch is benchmark row 4 of agent-vcs.md: the daemon's
// cache hit against a long-lived `git cat-file --batch` on a local clone, same
// blob, one core. Run both sides interleaved, only at a 1-minute AND 5-minute
// load of at most 8:
//
//	go test -run '^$' -bench 'ReadBlobHit|VsGit' -count 10 -cpu 1 ./internal/objcache
//
// It reads the module's own go.mod blob from the repository the tests run in
// and skips where git or a repository is not available.
func BenchmarkVsGitCatFileBatch(b *testing.B) {
	id, err := exec.Command("git", "rev-parse", "HEAD:go.mod").Output()
	if err != nil {
		b.Skip("no git repository with go.mod at HEAD")
	}
	oid := strings.TrimSpace(string(id))
	cmd := exec.Command("git", "cat-file", "--batch")
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		b.Skip("git cat-file unavailable")
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	out := bufio.NewReaderSize(outp, 1<<16)
	var size int
	read := func() []byte {
		fmt.Fprintln(in, oid)
		line, err := out.ReadString('\n')
		if err != nil {
			b.Fatal(err)
		}
		f := strings.Fields(line)
		size, _ = strconv.Atoi(f[2])
		buf := make([]byte, size+1)
		for n := 0; n < len(buf); {
			m, err := out.Read(buf[n:])
			if err != nil {
				b.Fatal(err)
			}
			n += m
		}
		return buf[:size]
	}
	data := read()
	c := New(Limits{})
	if err := c.Admit(oid, Blob, data); err != nil {
		b.Fatalf("git's blob does not verify against its own id: %v", err)
	}
	b.Run("git-cat-file-batch", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !bytes.Equal(read(), data) {
				b.Fatal("different bytes")
			}
		}
	})
	b.Run("ReadBlobHit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, d, ok := c.Get(oid); !ok || !bytes.Equal(d, data) {
				b.Fatal("miss")
			}
		}
	})
}
