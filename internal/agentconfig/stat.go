package agentconfig

import (
	"io/fs"
	"syscall"
)

// rawStat is the part of struct stat that Python's _identity and mode checks use.
type rawStat struct {
	dev, ino, mode, nlink, size, mtimeNs, ctimeNs int64
	uid, gid                                      int
}

const (
	sIFMT  = 0o170000
	sIFDIR = 0o040000
	sIFREG = 0o100000
	sIFLNK = 0o120000
	sISUID = 0o4000
	sISGID = 0o2000
)

func (s rawStat) isDir() bool  { return s.mode&sIFMT == sIFDIR }
func (s rawStat) isReg() bool  { return s.mode&sIFMT == sIFREG }
func (s rawStat) isLink() bool { return s.mode&sIFMT == sIFLNK }

// identity is Python's _identity(st): [dev, ino, mode, size, mtime_ns, ctime_ns].
func (s rawStat) identity() [6]int64 {
	return [6]int64{s.dev, s.ino, s.mode, s.size, s.mtimeNs, s.ctimeNs}
}

func statOf(fi fs.FileInfo) rawStat {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return sysStat(st)
	}
	// No raw stat: rebuild the type and permission bits from the FileInfo.
	mode := int64(fi.Mode().Perm())
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		mode |= sIFLNK
	case fi.IsDir():
		mode |= sIFDIR
	case fi.Mode().IsRegular():
		mode |= sIFREG
	}
	if fi.Mode()&fs.ModeSetuid != 0 {
		mode |= sISUID
	}
	if fi.Mode()&fs.ModeSetgid != 0 {
		mode |= sISGID
	}
	return rawStat{mode: mode, nlink: 1, size: fi.Size(), mtimeNs: fi.ModTime().UnixNano()}
}

// identityValue is the JSON-safe form stored in a profile's _identity table.
func identityValue(id [6]int64) []any {
	out := make([]any, len(id))
	for i, v := range id {
		out[i] = v
	}
	return out
}

// sameIdentity compares stored identity tables exactly (int64, not float64).
func sameIdentity(a, b any) bool {
	ma, oka := a.(map[string]any)
	mb, okb := b.(map[string]any)
	if !oka || !okb || len(ma) != len(mb) {
		return false
	}
	for k, va := range ma {
		vb, ok := mb[k]
		if !ok || !sameIDList(va, vb) {
			return false
		}
	}
	return true
}

func sameIDList(a, b any) bool {
	la, lb := asInt64s(a), asInt64s(b)
	if la == nil || lb == nil || len(la) != len(lb) {
		return false
	}
	for i := range la {
		if la[i] != lb[i] {
			return false
		}
	}
	return true
}

func asInt64s(v any) []int64 {
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case [6]int64:
		return t[:]
	case []int64:
		return t
	default:
		return nil
	}
	out := make([]int64, len(items))
	for i, x := range items {
		switch n := x.(type) {
		case int64:
			out[i] = n
		case int:
			out[i] = int64(n)
		case float64:
			out[i] = int64(n)
		default:
			return nil
		}
	}
	return out
}
