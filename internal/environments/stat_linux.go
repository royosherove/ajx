package environments

import "syscall"

func sysStat(st *syscall.Stat_t) statInfo {
	return statInfo{dev: int64(st.Dev), ino: uint64(st.Ino), mode: uint32(st.Mode), size: st.Size,
		nlink: uint64(st.Nlink), uid: st.Uid, gid: st.Gid, mtimeNs: st.Mtim.Nano(), ctimeNs: st.Ctim.Nano()}
}
