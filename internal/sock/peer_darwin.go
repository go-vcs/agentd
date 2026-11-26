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
			var x *unix.Xucred
			x, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
			if cerr == nil {
				uid = x.Uid
			}
		}); err != nil {
			return 0, err
		}
		return uid, cerr
	}
}
