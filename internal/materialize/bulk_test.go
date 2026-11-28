package materialize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
)

// pfetch records what a bulk path is told, and counts blobs read before they
// were announced.
type pfetch struct {
	*fake
	chunks   [][]string
	announce map[string]bool
	err      error
	missed   int
}

func (p *pfetch) Prefetch(_ context.Context, ids []string) error {
	if p.err != nil {
		return p.err
	}
	p.chunks = append(p.chunks, append([]string(nil), ids...))
	for _, id := range ids {
		p.announce[id] = true
	}
	return nil
}

func (p *pfetch) Blob(ctx context.Context, id string) ([]byte, error) {
	if !p.announce[id] {
		p.missed++
	}
	return p.fake.Blob(ctx, id)
}

func manyBlobs(n int) *pfetch {
	f := &fake{trees: map[string][]gittree.Entry{}, blobs: map[string][]byte{}}
	var es []gittree.Entry
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("b%03d", i)
		f.blobs[id] = []byte(id)
		es = append(es, gittree.Entry{Mode: "100644", Name: fmt.Sprintf("f%03d", i), ID: id})
	}
	f.trees["T0"] = es
	return &pfetch{fake: f, announce: map[string]bool{}}
}

func TestBulkPathIsToldEveryBlobInChunksBeforeItIsRead(t *testing.T) {
	root, dir := open(t)
	p := manyBlobs(70)
	res, err := Write(context.Background(), root, "w", "T0", nil, p, DefaultLimits())
	if err != nil || res.Files != 70 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(p.chunks) != 3 || len(p.chunks[0]) != PrefetchChunk || len(p.chunks[1]) != PrefetchChunk || len(p.chunks[2]) != 70-2*PrefetchChunk {
		t.Fatalf("chunks %d", len(p.chunks))
	}
	if p.missed != 0 {
		t.Fatalf("%d blobs read before they were announced", p.missed)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "w", "f069")); err != nil || string(b) != "b069" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestBulkPathErrorIsReportedAndLeavesNothing(t *testing.T) {
	root, dir := open(t)
	p := manyBlobs(5)
	p.err = aerr.New(aerr.RateLimited, "slow down")
	if _, err := Write(context.Background(), root, "w", "T0", nil, p, DefaultLimits()); reason(err) != aerr.RateLimited {
		t.Fatalf("%v", err)
	}
	if exists(filepath.Join(dir, "w")) {
		t.Fatal("a failed checkout left its directory")
	}
}

func TestFileBoundCountsBeforeAnyBlobIsFetched(t *testing.T) {
	root, _ := open(t)
	p := manyBlobs(10)
	lim := DefaultLimits()
	lim.MaxFiles = 9
	if _, err := Write(context.Background(), root, "w", "T0", nil, p, lim); reason(err) != aerr.BoundExceeded {
		t.Fatalf("%v", err)
	}
	if len(p.chunks) != 0 || p.fake.blobCalls != 0 {
		t.Fatalf("fetched %d blobs for a checkout the bound refuses", p.fake.blobCalls)
	}
}
