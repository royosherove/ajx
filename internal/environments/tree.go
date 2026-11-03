package environments

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// statInfo is the subset of struct stat the inspection compares.
type statInfo struct {
	dev     int64
	ino     uint64
	mode    uint32
	size    int64
	nlink   uint64
	uid     uint32
	gid     uint32
	mtimeNs int64
	ctimeNs int64
}

func (s statInfo) format() uint32 { return s.mode & syscall.S_IFMT }
func (s statInfo) isDir() bool    { return s.format() == syscall.S_IFDIR }
func (s statInfo) isReg() bool    { return s.format() == syscall.S_IFREG }
func (s statInfo) isLink() bool   { return s.format() == syscall.S_IFLNK }

func infoStat(fi fs.FileInfo) (statInfo, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return statInfo{}, &fs.PathError{Op: "stat", Path: fi.Name(), Err: syscall.ENOTSUP}
	}
	return sysStat(st), nil
}

func realFstat(f *os.File) (statInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return statInfo{}, err
	}
	return infoStat(fi)
}

func pathStat(path string) (statInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return statInfo{}, err
	}
	return infoStat(fi)
}

// Operating-system hooks (Python tests patch os.fstat, os.fchown, os.open,
// time.sleep and friends; the Go tests replace these).
var (
	fstat      = realFstat
	fchown     = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
	getuid     = os.Getuid
	geteuid    = os.Geteuid
	getgid     = os.Getgid
	sleep      = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	beforeOpen = func(name string) {}
	// ownershipMount identifies the open mount, including same-device Linux bind mounts.
	ownershipMount = realOwnershipMount
	// chownOwnedTree walks open descriptors only; never chowns a link, shared inode or another mount.
	chownOwnedTree = realChownOwnedTree
	// discardPartialCopy removes the private, partially written snapshot an inspection created.
	discardPartialCopy = realDiscardPartialCopy
)

// statChanges names the requested stat attributes that differ between two inspections of one entry.
func statChanges(before, after statInfo, attributes ...string) []string {
	var out []string
	for _, name := range attributes {
		var same bool
		switch name {
		case "dev/ino":
			same = before.dev == after.dev && before.ino == after.ino
		case "type":
			same = before.format() == after.format()
		case "size":
			same = before.size == after.size
		case "mtime":
			same = before.mtimeNs == after.mtimeNs
		case "ctime":
			same = before.ctimeNs == after.ctimeNs
		case "nlink":
			same = before.nlink == after.nlink
		}
		if !same {
			out = append(out, name)
		}
	}
	return out
}

// dirHandle is an open directory: a Root for name-relative operations and a
// File on the same directory for fstat, listing and fchown/fchmod.
type dirHandle struct {
	root *os.Root
	file *os.File
}

func (d *dirHandle) close() {
	if d == nil {
		return
	}
	if d.file != nil {
		d.file.Close()
	}
	if d.root != nil {
		d.root.Close()
	}
}

func handleFor(root *os.Root) (*dirHandle, error) {
	f, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	return &dirHandle{root: root, file: f}, nil
}

func loopError(name string) error {
	return &fs.PathError{Op: "openat", Path: name, Err: syscall.ELOOP}
}

// noFollow emulates O_NOFOLLOW for an os.Root open: a symlink is refused
// with ELOOP. os.Root never follows a link outside the parent; an entry
// replaced between the check and the open is detected by identity.
func noFollow(parent *os.Root, name string, open func() (*os.File, func(), error)) (*os.File, func(), error) {
	beforeOpen(name)
	fi, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, loopError(name)
	}
	before, err := infoStat(fi)
	if err != nil {
		return nil, nil, err
	}
	f, closer, err := open()
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes") {
			return nil, nil, loopError(name)
		}
		return nil, nil, err
	}
	after, err := realFstat(f)
	if err != nil {
		closer()
		return nil, nil, err
	}
	if after.dev != before.dev || after.ino != before.ino {
		if now, err := parent.Lstat(name); err == nil && now.Mode()&fs.ModeSymlink != 0 {
			closer()
			return nil, nil, loopError(name)
		}
	}
	return f, closer, nil
}

// openChildDir is os.open(name, O_RDONLY|O_DIRECTORY|O_NOFOLLOW, dir_fd=parent).
func openChildDir(parent *os.Root, name string) (*dirHandle, error) {
	var handle *dirHandle
	_, _, err := noFollow(parent, name, func() (*os.File, func(), error) {
		root, err := parent.OpenRoot(name)
		if err != nil {
			return nil, nil, err
		}
		h, err := handleFor(root)
		if err != nil {
			return nil, nil, err
		}
		handle = h
		return h.file, h.close, nil
	})
	if err != nil {
		return nil, err
	}
	return handle, nil
}

// openChildFile is os.open(name, flags|O_NOFOLLOW, dir_fd=parent).
func openChildFile(parent *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	f, _, err := noFollow(parent, name, func() (*os.File, func(), error) {
		f, err := parent.OpenFile(name, flags|syscall.O_NOFOLLOW, perm)
		if err != nil {
			return nil, nil, err
		}
		return f, func() { f.Close() }, nil
	})
	return f, err
}

// openDirectory opens each component relative to an already open parent,
// never following symlinks (Python _open_directory).
func openDirectory(path string) (*dirHandle, error) {
	root, err := os.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	current, err := handleFor(root)
	if err != nil {
		return nil, err
	}
	parts := pathParts(path)
	if len(parts) > 0 && parts[0] == "/" {
		parts = parts[1:]
	}
	for _, component := range parts {
		child, err := openChildDir(current.root, component)
		current.close()
		if err != nil {
			return nil, err
		}
		current = child
	}
	return current, nil
}

type mountID struct {
	dev    int64
	mnt    int64
	hasMnt bool
	label  string // tests: a synthetic mount identity
}

func realOwnershipMount(f *os.File) (mountID, error) {
	info, err := fstat(f)
	if err != nil {
		return mountID{}, err
	}
	if runtime.GOOS != "linux" {
		return mountID{dev: info.dev}, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", f.Fd()))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "mnt_id:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						return mountID{dev: info.dev, mnt: n, hasMnt: true}, nil
					}
				}
			}
		}
	}
	return mountID{}, envError("Cannot verify host mount boundaries before restoring worker ownership")
}

func realChownOwnedTree(d *dirHandle, uid, gid int) error {
	boundary, err := ownershipMount(d.file)
	if err != nil {
		return err
	}
	var visit func(f *os.File, root *os.Root) error
	visit = func(f *os.File, root *os.Root) error {
		info, err := fstat(f)
		if err != nil {
			return err
		}
		directory := info.isDir()
		if !directory && (!info.isReg() || info.nlink != 1) {
			return nil
		}
		m, err := ownershipMount(f)
		if err != nil {
			return err
		}
		if m != boundary {
			return nil
		}
		if int(info.uid) != uid || int(info.gid) != gid {
			if err := fchown(f, uid, gid); err != nil {
				return err
			}
		}
		if !directory {
			return nil
		}
		names, err := f.Readdirnames(-1)
		if err != nil {
			return err
		}
		for _, name := range names {
			fi, err := root.Lstat(name)
			if err != nil {
				return err
			}
			entry, err := infoStat(fi)
			if err != nil {
				return err
			}
			isDirectory := entry.isDir()
			if entry.dev != info.dev || (!isDirectory && (!entry.isReg() || entry.nlink != 1)) {
				continue
			}
			if err := func() error {
				var child *os.File
				var childRoot *os.Root
				if isDirectory {
					h, err := openChildDir(root, name)
					if err != nil {
						return err
					}
					defer h.close()
					child, childRoot = h.file, h.root
				} else {
					cf, err := openChildFile(root, name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
					if err != nil {
						return err
					}
					defer cf.Close()
					child = cf
				}
				opened, err := fstat(child)
				if err != nil {
					return err
				}
				if opened.dev != entry.dev || opened.ino != entry.ino || opened.format() != entry.format() {
					return envError("Attempt tree changed while restoring worker ownership")
				}
				return visit(child, childRoot)
			}(); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(d.file, d.root)
}

type treeOpts struct {
	fixtures    bool
	destination string
}

// treeManifest bounds/hashes and optionally copies a tree using no-follow
// descriptor-relative reads (Python _tree_manifest). An entry observed
// changing mid-inspection is named with the attributes that differed; the
// whole inspection then reruns once from scratch (after discarding any
// partial copy). A change observed again fails.
func treeManifest(root, field string, o treeOpts) (util.M, error) {
	root, err := directory(root, field, true)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		out, err := inspectTree(root, field, o)
		if err == nil {
			return out, nil
		}
		if !isKind(err, KindInspectionChanged) {
			return nil, err
		}
		e, _ := asError(err)
		if attempt > 1 {
			return nil, &Error{Kind: KindInspectionChanged, Msg: e.Msg + " (reproduced on re-inspection)"}
		}
		if o.destination != "" && !discardPartialCopy(o.destination) {
			return nil, err
		}
		sleep(reinspectDelay)
	}
}

func realDiscardPartialCopy(destination string) bool {
	if err := os.RemoveAll(destination); err != nil {
		return false
	}
	return !lexists(destination)
}

func inspectTree(root, field string, o treeOpts) (util.M, error) {
	var entries [][]any
	var total int64
	var visit func(dir *dirHandle, prefix string, dest *dirHandle) error
	visit = func(dir *dirHandle, prefix string, dest *dirHandle) error {
		names, err := dir.file.Readdirnames(-1)
		if err != nil {
			return err
		}
		sort.Strings(names)
		for _, name := range names {
			rel := prefix + name
			if o.fixtures && forbidden(rel) {
				return fail(field, "fixture tree contains a credential/configuration/report path")
			}
			fi, err := dir.root.Lstat(name)
			if err != nil {
				return err
			}
			info, err := infoStat(fi)
			if err != nil {
				return err
			}
			switch {
			case info.isLink():
				return fail(field, "symlinks in starting directories are not permitted")
			case info.isDir():
				entries = append(entries, []any{rel + "/", "directory"})
				if strings.Count(rel, "/") >= 64 || len(entries) > maxFiles {
					return fail(field, "starting directory exceeds the nesting/entry limit")
				}
				if err := func() error {
					child, err := openChildDir(dir.root, name)
					if err != nil {
						return err
					}
					defer child.close()
					opened, err := fstat(child.file)
					if err != nil {
						return err
					}
					if differences := statChanges(info, opened, "dev/ino"); len(differences) > 0 {
						return changed(field, rel+"/", differences)
					}
					var output *dirHandle
					if dest != nil {
						if err := dest.root.Mkdir(name, 0o700); err != nil {
							return err
						}
						if output, err = openChildDir(dest.root, name); err != nil {
							return err
						}
						defer output.close()
					}
					return visit(child, rel+"/", output)
				}(); err != nil {
					return err
				}
			case info.isReg():
				if info.nlink != 1 {
					return fail(field, "hardlinked files are not permitted")
				}
				total += info.size
				if total > maxBytes {
					return fail(field, fmt.Sprintf("starting directory exceeds %d bytes", maxBytes))
				}
				digest, err := copyRegular(dir, dest, name, rel, field, info)
				if err != nil {
					return err
				}
				entries = append(entries, []any{rel, info.size, info.mode&0o111 != 0, digest})
			default:
				return fail(field, "sockets, devices, FIFOs, and other special files are not permitted")
			}
			if len(entries) > maxFiles {
				return fail(field, fmt.Sprintf("starting directory exceeds %d entries", maxFiles))
			}
		}
		return nil
	}
	err := func() error {
		input, err := openDirectory(root)
		if err != nil {
			return err
		}
		defer input.close()
		var output *dirHandle
		if o.destination != "" {
			destination := pyPath(o.destination)
			if err := os.MkdirAll(parentOf(destination), 0o700); err != nil {
				return err
			}
			if err := os.Mkdir(destination, 0o700); err != nil {
				return err
			}
			if output, err = openDirectory(destination); err != nil {
				return err
			}
			defer output.close()
		}
		return visit(input, "", output)
	}()
	if err != nil {
		if e, ok := asError(err); ok {
			return nil, e
		}
		return nil, fail(field, "cannot safely inspect/copy directory ("+osErrorName(err)+"); it may have changed")
	}
	sum := sha256.Sum256([]byte(pyDumps(sortedEntries(entries), true)))
	return util.M{"sha256": hex.EncodeToString(sum[:]), "entries": int64(len(entries)), "bytes": total}, nil
}

// copyRegular hashes (and copies into dest) one regular file, checking that
// it does not change while it is read.
func copyRegular(dir, dest *dirHandle, name, rel, field string, info statInfo) (string, error) {
	stream, err := openChildFile(dir.root, name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	opened, err := fstat(stream)
	if err != nil {
		return "", err
	}
	if differences := statChanges(info, opened, "dev/ino", "type", "size", "nlink"); len(differences) > 0 {
		return "", changed(field, rel, differences)
	}
	digest := sha256.New()
	var count int64
	var output *os.File
	if dest != nil {
		mode := os.FileMode(0o600)
		if info.mode&0o111 != 0 {
			mode = 0o700
		}
		output, err = dest.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
		if err != nil {
			return "", err
		}
	}
	result, err := func() (string, error) {
		buf := make([]byte, 65536)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				count += int64(n)
				if count > info.size {
					return "", changed(field, rel, []string{"size (grew during the read)"})
				}
				digest.Write(buf[:n])
				if output != nil {
					if _, werr := output.Write(buf[:n]); werr != nil {
						return "", werr
					}
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return "", rerr
			}
		}
		after, err := fstat(stream)
		if err != nil {
			return "", err
		}
		differences := statChanges(info, after, "size", "mtime", "ctime", "nlink")
		if count != info.size && !util.Contains(differences, "size") {
			differences = append([]string{"size"}, differences...)
		}
		if len(differences) > 0 {
			return "", changed(field, rel, differences)
		}
		return hex.EncodeToString(digest.Sum(nil)), nil
	}()
	if output != nil {
		if cerr := output.Close(); cerr != nil && err == nil {
			return "", cerr
		}
	}
	return result, err
}

// privateHostDirectories creates/checks private directories relative to an
// open report root, without symlink traversal (Python _private_host_directories).
func privateHostDirectories(runDir string, paths []string) error {
	anchor, err := openDirectory(runDir)
	if err != nil {
		return fail("environment.host_env", "private check-context roots must be directories without symlinks ("+
			osErrorName(err)+")")
	}
	defer anchor.close()
	unique := util.Uniq(func() []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = pyPath(p)
		}
		return out
	}())
	for _, path := range unique {
		if err := func() error {
			current := anchor
			defer func() {
				if current != anchor {
					current.close()
				}
			}()
			for _, component := range pathParts(relPath(path, runDir)) {
				if component == "." {
					continue
				}
				if err := current.root.Mkdir(component, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
					return err
				}
				child, err := openChildDir(current.root, component)
				if err != nil {
					return err
				}
				if current != anchor {
					current.close()
				}
				current = child
				info, err := fstat(current.file)
				if err != nil {
					return err
				}
				if int(info.uid) != getuid() {
					return fail("environment.host_env", "private check-context directories must be coordinator-owned")
				}
				if err := current.file.Chmod(0o700); err != nil {
					return err
				}
			}
			return nil
		}(); err != nil {
			if e, ok := asError(err); ok {
				return e
			}
			return fail("environment.host_env", "private check-context roots must be directories without symlinks ("+
				osErrorName(err)+")")
		}
	}
	return nil
}
