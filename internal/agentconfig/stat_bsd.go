//go:build darwin || freebsd || netbsd

package agentconfig

import "syscall"

func sysStat(st *syscall.Stat_t) rawStat {
	return rawStat{dev: int64(st.Dev), ino: int64(st.Ino), mode: int64(st.Mode), nlink: int64(st.Nlink),
		size: st.Size, mtimeNs: st.Mtimespec.Nano(), ctimeNs: st.Ctimespec.Nano(), uid: int(st.Uid), gid: int(st.Gid)}
}
