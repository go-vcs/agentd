//go:build windows

package objcache

import "os"

// dirOpenToOthers is always false on Windows. Go reports the mode bits of every
// Windows directory as 0777, so they say nothing about who can open it, and the
// owner-only check is NOT done there: the directory's access control list is not
// read. This is a documented gap (README, "Disk tier"), not a verification.
func dirOpenToOthers(os.FileInfo) bool { return false }
