package runner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/royosherove/ajx/internal/util"
)

// pinnedDir is an owned directory opened without following symlinks; its
// descriptor stays valid if the path is later replaced.
type pinnedDir struct {
	root *os.Root
	info os.FileInfo
}

func (p *pinnedDir) Close() { _ = p.root.Close() }

// afterOpenDir is called with each directory just pinned (tests race a
// replacement against it).
var afterOpenDir func(info os.FileInfo)

func notePinned(info os.FileInfo) {
	if afterOpenDir != nil {
		afterOpenDir(info)
	}
}

// openChildNofollow opens name inside dir as a pinned directory: a symlink or a
// replacement since the lstat is refused, never followed.
func openChildNofollow(dir *os.Root, name, display string) (*os.Root, os.FileInfo, error) {
	st, err := dir.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, nil, &fs.PathError{Op: "open", Path: display, Err: syscall.ELOOP}
	}
	if !st.IsDir() {
		return nil, nil, &fs.PathError{Op: "open", Path: display, Err: syscall.ENOTDIR}
	}
	child, err := dir.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := child.Stat(".")
	if err != nil {
		child.Close()
		return nil, nil, err
	}
	if !os.SameFile(st, opened) {
		child.Close()
		return nil, nil, &fs.PathError{Op: "open", Path: display, Err: syscall.ELOOP}
	}
	notePinned(opened)
	return child, opened, nil
}

// openDirectoryNofollow opens every component relative to its pinned parent
// and never resolves a symlink (Python _open_directory_nofollow).
func openDirectoryNofollow(path string) (*pinnedDir, error) {
	if !filepath.IsAbs(path) || util.Contains(strings.Split(filepath.ToSlash(path), "/"), "..") {
		return nil, runtimeErr("credential roots must be absolute owned paths without parent traversal")
	}
	sep := string(filepath.Separator)
	cur, err := os.OpenRoot(sep)
	if err != nil {
		return nil, err
	}
	info, err := cur.Stat(".")
	if err != nil {
		cur.Close()
		return nil, err
	}
	shown := sep
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), sep), sep) {
		if part == "" || part == "." {
			continue
		}
		shown = filepath.Join(shown, part)
		next, nextInfo, err := openChildNofollow(cur, part, shown)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur, info = next, nextInfo
	}
	return &pinnedDir{root: cur, info: info}, nil
}

// purgeCredentialTree unlinks known credential names only inside pinned
// directories, including a symlink entry itself; never follows a symlink.
func purgeCredentialTree(dir *os.Root) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		st, err := dir.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
			child, _, err := openChildNofollow(dir, name, name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err == nil {
				perr := purgeCredentialTree(child)
				child.Close()
				if perr != nil {
					return perr
				}
				continue
			}
			if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
				return err
			}
			// Replaced by a symlink or a file since the listing: unlink it only if it has a known name.
		}
		if util.Contains(CredentialFiles, name) {
			if err := dir.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
