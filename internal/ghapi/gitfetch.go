package ghapi

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/gitproto/pktline"
)

// A commit's raw bytes are not in the REST answer: git/commits returns the
// dates normalised to UTC (the offset git hashed is gone), strips the final
// newline of the message and shortens the gpgsig header. So the commit is
// asked of the git smart-HTTP endpoint instead, which sends the object itself
// (gitprotocol-v2 "fetch", gitformat-pack). One request, one object:
//
//	want <id>, deepen 1, filter tree:0, done
//
// returns a pack holding just that commit. The bytes are NOT trusted here: the
// daemon's cache recomputes the id before it keeps or returns anything.

const (
	// maxFetchWire bounds the response: a commit of MaxObject bytes deflated
	// (never larger than a little over its size), the sideband and pack framing,
	// and the section headers.
	maxFetchWire = MaxObject + 1<<20
	packCommit   = 1 // OBJ_COMMIT, gitformat-pack
)

var errPack = errors.New("pack: not one whole commit")

// fetchRequest is the body of a v2 "fetch" for one commit.
func fetchRequest(id string) []byte {
	var b bytes.Buffer
	w := pktline.NewWriter(&b)
	format := "sha1"
	if len(id) == 64 {
		format = "sha256"
	}
	// A pkt-line holding a short ASCII line cannot fail to be written to a Buffer.
	_ = w.Text("command=fetch")
	_ = w.Text("agent=forge-agentd")
	_ = w.Text("object-format=" + format)
	_ = w.Delim()
	_ = w.Text("no-progress")
	_ = w.Text("filter tree:0")
	_ = w.Text("want " + id)
	_ = w.Text("deepen 1")
	_ = w.Text("done")
	_ = w.Flush()
	return b.Bytes()
}

func (c *Client) fetchCommit(ctx context.Context, repo, id string) ([]byte, error) {
	u := *c.git
	u.Path = strings.TrimRight(c.git.Path, "/") + "/" + repo + ".git/git-upload-pack"
	// u was validated by SetGitURL and repo by ValidRepo: the request cannot fail
	// to build.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(fetchRequest(id)))
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.token)))
	req.Header.Set("User-Agent", "forge-agentd")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, aerr.New(aerr.Unavailable, "upstream unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return nil, statusError(resp, b)
	}
	lr := &io.LimitedReader{R: resp.Body, N: maxFetchWire + 1}
	raw, err := readCommitPack(lr, id)
	switch {
	case lr.N <= 0: // the answer ran past the wire bound, whatever was read of it
		return nil, aerr.New(aerr.BoundExceeded, "upstream answer larger than "+strconv.Itoa(maxFetchWire)+" bytes")
	case err == nil:
		return raw, nil
	case errors.Is(err, errBlobTooLarge):
		return nil, aerr.New(aerr.BoundExceeded, "commit larger than "+strconv.Itoa(MaxObject)+" bytes")
	case errors.Is(err, errNotOurRef):
		return nil, aerr.New(aerr.NotFound, "commit not found")
	}
	return nil, aerr.New(aerr.Unavailable, "upstream answer unreadable")
}

var errNotOurRef = errors.New("upstream does not have the commit")

// readCommitPack reads a v2 fetch response and returns the one commit its pack
// holds. Sections other than "packfile" (shallow-info) are skipped; the
// packfile section is demultiplexed (band 1 data, 2 progress, 3 fatal).
func readCommitPack(r io.Reader, id string) ([]byte, error) {
	pr := pktline.NewReader(r)
	var pack []byte
	in := false
	// Every packet is at least 4 bytes and the reader is bounded: the loop ends.
	for n := 0; n <= maxFetchWire/4; n++ {
		p, err := pr.Read()
		if err != nil {
			if err == io.EOF && in && len(pack) > 0 {
				break
			}
			return nil, err
		}
		if p.Kind == pktline.Flush || p.Kind == pktline.ResponseEnd {
			if in {
				break
			}
			return nil, errPack
		}
		if p.Kind == pktline.Delim {
			continue
		}
		if !in {
			line := string(pktline.Text(p.Data))
			switch {
			case strings.HasPrefix(line, "ERR "):
				if strings.Contains(line, "not our ref") {
					return nil, errNotOurRef
				}
				return nil, errPack
			case line == "packfile":
				in = true
			}
			continue
		}
		// The response is bounded by the caller's LimitReader, and so is the pack.
		band, data := byte(0), []byte(nil) // an empty packet has no band: refused below
		if len(p.Data) > 0 {
			band, data = p.Data[0], p.Data[1:]
		}
		switch band {
		case 1:
			pack = append(pack, data...)
		case 2:
			// progress: ignored
		default:
			return nil, errPack
		}
	}
	return parseCommitPack(pack, len(id))
}

// parseCommitPack reads a pack that must hold exactly one whole (not deltified)
// commit and returns its bytes. idLen is 40 (sha1) or 64 (sha256): the length of
// the trailing checksum follows from it.
func parseCommitPack(pack []byte, idLen int) ([]byte, error) {
	sum := 20
	if idLen == 64 {
		sum = 32
	}
	// "PACK", version, count, then at least one entry byte and the checksum.
	if len(pack) < 12+1+sum || string(pack[:4]) != "PACK" {
		return nil, errPack
	}
	if v := be32(pack[4:]); v != 2 && v != 3 {
		return nil, errPack
	}
	if be32(pack[8:]) != 1 {
		return nil, errPack
	}
	body, trailer := pack[:len(pack)-sum], pack[len(pack)-sum:]
	var got []byte
	if sum == 20 {
		h := sha1.Sum(body)
		got = h[:]
	} else {
		h := sha256.Sum256(body)
		got = h[:]
	}
	if !bytes.Equal(got, trailer) {
		return nil, errPack
	}
	// n-byte type and length: 3-bit type, 4 low bits of the size, then 7 bits a
	// byte while the high bit is set.
	i := 12
	c := body[i]
	i++
	if c>>4&7 != packCommit {
		return nil, errPack
	}
	size := uint64(c & 15)
	for shift := uint(4); c&0x80 != 0; shift += 7 {
		if i >= len(body) || shift > 28 {
			return nil, errPack
		}
		c = body[i]
		i++
		size |= uint64(c&0x7f) << shift
	}
	if size > MaxObject { // DecodeCommit refuses len(raw) > MaxObject: the same bound
		return nil, errBlobTooLarge
	}
	br := bytes.NewReader(body[i:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, errPack
	}
	out, err := io.ReadAll(io.LimitReader(zr, int64(size)+1))
	if err != nil || uint64(len(out)) != size {
		return nil, errPack
	}
	// The stream must end exactly where the checksum begins: the zlib reader
	// reads a bytes.Reader byte by byte, so nothing is consumed past its end.
	if err := zr.Close(); err != nil || br.Len() != 0 {
		return nil, errPack
	}
	return out, nil
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
