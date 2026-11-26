//go:build linux || darwin

package sock

import (
	"fmt"
	"os"
	"syscall"
)

func checkDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("socket directory %s is not owned by this user", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket directory %s has mode %04o: others could reach it; chmod 700", dir, st.Mode().Perm())
	}
	return nil
}
