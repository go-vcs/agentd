//go:build !windows

package objcache

import "os"

// dirOpenToOthers reports whether the directory is accessible to its group or
// to everyone: the strict owner-only (0700) rule of the disk tier.
func dirOpenToOthers(fi os.FileInfo) bool { return fi.Mode().Perm()&0o077 != 0 }
