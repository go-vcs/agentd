//go:build !windows

package objcache

import (
	"os"
	"testing"
)

func TestDirOpenToOthersUnix(t *testing.T) {
	for _, c := range []struct {
		mode os.FileMode
		want bool
	}{{0o700, false}, {0o750, true}, {0o705, true}, {0o755, true}, {0o770, true}, {0o777, true}} {
		dir := t.TempDir()
		if err := os.Chmod(dir, c.mode); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := dirOpenToOthers(fi); got != c.want {
			t.Errorf("mode %o: open to others = %v, want %v", c.mode, got, c.want)
		}
	}
}
