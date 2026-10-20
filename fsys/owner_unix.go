//go:build unix

package fsys

import (
	"os"
	"syscall"
)

// preserveOwner gives f the owner and group of like when that differs from the
// current process and the kernel allows it (that is, when running as root or
// as the owner). Failure is ignored, as it is for sed -i and cp -p.
func preserveOwner(f *os.File, like os.FileInfo) {
	st, ok := like.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if int(st.Uid) == os.Getuid() && int(st.Gid) == os.Getgid() {
		return
	}
	_ = f.Chown(int(st.Uid), int(st.Gid))
}
