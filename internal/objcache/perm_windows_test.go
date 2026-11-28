//go:build windows

package objcache

import (
	"os"
	"testing"
)

// On Windows the mode check is skipped (documented gap): a directory whose mode
// bits read 0777 is accepted, and OpenDisk succeeds on a fresh one.
func TestDirOpenToOthersWindowsSkipped(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dirOpenToOthers(fi) {
		t.Error("the mode check ran on Windows")
	}
	d, err := OpenDisk(t.TempDir()+`\cache`, DiskLimits{})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}
