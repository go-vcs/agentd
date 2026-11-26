package sock

import (
	"net"

	"golang.org/x/sys/unix"
)

func init() {
	peerUID = func(c *net.UnixConn) (uint32, error) {
		raw, err := c.SyscallConn()
		if err != nil {
			return 0, err
		}
		var uid uint32
		var cerr error
		if err := raw.Control(func(fd uintptr) {
			var u *unix.Ucred
			u, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
			if cerr == nil {
				uid = u.Uid
			}
		}); err != nil {
			return 0, err
		}
		return uid, cerr
	}
}
