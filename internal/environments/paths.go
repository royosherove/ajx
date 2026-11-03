package environments

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// pyPath normalizes a path like pathlib.Path: repeated separators and "."
// components are dropped, ".." is kept (unlike filepath.Clean).
func pyPath(p string) string {
	if p == "" {
		return "."
	}
	parts := pathParts(p)
	if len(parts) == 0 {
		return "."
	}
	if parts[0] == "/" {
		return "/" + strings.Join(parts[1:], "/")
	}
	return strings.Join(parts, "/")
}

// pathParts is PurePosixPath(p).parts.
func pathParts(p string) []string {
	var parts []string
	if strings.HasPrefix(p, "/") {
		parts = append(parts, "/")
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		parts = append(parts, c)
	}
	return parts
}

func pathJoin(base string, elems ...string) string {
	out := pyPath(base)
	for _, e := range elems {
		if strings.HasPrefix(e, "/") {
			out = pyPath(e)
			continue
		}
		if out == "/" {
			out = pyPath("/" + e)
		} else {
			out = pyPath(out + "/" + e)
		}
	}
	return out
}

func isAbs(p string) bool { return strings.HasPrefix(p, "/") }

// parentOf is Path(p).parent.
func parentOf(p string) string {
	p = pyPath(p)
	i := strings.LastIndex(p, "/")
	switch {
	case p == "/" || p == ".":
		return p
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return p[:i]
}

// parentsOf is (Path(p), *Path(p).parents) for an absolute path.
func parentsOf(p string) []string {
	p = pyPath(p)
	out := []string{p}
	for p != "/" && p != "." {
		p = parentOf(p)
		out = append(out, p)
	}
	return out
}

func baseName(p string) string {
	p = pyPath(p)
	if p == "/" {
		return ""
	}
	return p[strings.LastIndex(p, "/")+1:]
}

// relativeTo is Path(a).is_relative_to(b) (lexical).
func relativeTo(a, b string) bool {
	pa, pb := pathParts(a), pathParts(b)
	if len(pb) > len(pa) {
		return false
	}
	for i := range pb {
		if pa[i] != pb[i] {
			return false
		}
	}
	return true
}

// relPath is str(Path(a).relative_to(b)); a must be relative to b.
func relPath(a, b string) string {
	pa, pb := pathParts(a), pathParts(b)
	rest := pa[len(pb):]
	if len(rest) == 0 {
		return "."
	}
	return strings.Join(rest, "/")
}

// overlaps is Python _overlaps.
func overlaps(a, b string) bool {
	return pyPath(a) == pyPath(b) || relativeTo(a, b) || relativeTo(b, a)
}

// realpath is Path(p).resolve() (non-strict): symlinks in the longest
// existing prefix are resolved; the missing remainder is appended.
func realpath(p string) string {
	if !isAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = wd + "/" + p
		}
	}
	p = filepath.Clean(pyPath(p))
	cur := p
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if len(rest) == 0 {
				return r
			}
			return filepath.Join(append([]string{r}, rest...)...)
		}
		if cur == "/" {
			return p
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = filepath.Dir(cur)
	}
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// pathText is Python _path_text for a string value.
func pathText(value any, field string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fail(field, "expected a directory path")
	}
	if text == "" || strings.ContainsAny(text, "\x00\n\r,\":") || strings.Contains(text, `\`) {
		return "", fail(field, "path is empty or contains unsupported control/mount-syntax characters")
	}
	for _, part := range pathParts(text) {
		if part == ".." {
			return "", fail(field, "path traversal and shell/environment expansion are not permitted")
		}
	}
	if strings.HasPrefix(text, "~") || strings.Contains(text, "$") {
		return "", fail(field, "path traversal and shell/environment expansion are not permitted")
	}
	return text, nil
}

// directory is Python _directory: reject user-controlled symlink components,
// including missing-leaf parents, then resolve.
func directory(value any, field string, exists bool) (string, error) {
	text, err := pathText(value, field)
	if err != nil {
		return "", err
	}
	path := pyPath(text)
	if !isAbs(path) {
		return "", fail(field, "expected an absolute path")
	}
	for _, part := range parentsOf(path) {
		// Standard macOS aliases are not user-selected fixture symlinks.
		if runtime.GOOS == "darwin" && (part == "/tmp" || part == "/var" || part == "/etc") &&
			realpath(part) == "/private"+part {
			continue
		}
		if isSymlink(part) {
			return "", fail(field, "symlink directory components are not permitted")
		}
	}
	path = realpath(path)
	if lexists(path) && !isDir(path) {
		return "", fail(field, "expected a directory, not a file, socket, or device")
	}
	if exists && !isDir(path) {
		return "", fail(field, "directory does not exist")
	}
	return path, nil
}

// forbidden is Python _forbidden.
func forbidden(path string) bool {
	for _, p := range pathParts(path) {
		l := strings.ToLower(p)
		if forbiddenNames[l] || strings.HasSuffix(l, ".sock") || strings.HasSuffix(l, ".pem") ||
			strings.HasSuffix(l, ".key") || strings.HasPrefix(l, ".env.") {
			return true
		}
	}
	return false
}

// target is Python _target.
func target(value any, field string) (string, error) {
	text, err := pathText(value, field)
	if err != nil {
		return "", err
	}
	switch {
	case !isAbs(text) || pyPath(text) != text || text == "/" || text == "/tmp" || text == "/var" ||
		text == "/home" || text == "/opt":
		return "", fail(field, "expected a normalized absolute directory target, not a filesystem root")
	}
	for _, p := range systemTargets {
		if overlaps(text, p) {
			return "", fail(field, "target overlaps a system, credential, engine, or report path")
		}
	}
	if forbidden(text) {
		return "", fail(field, "target overlaps a system, credential, engine, or report path")
	}
	return text, nil
}

// dockerHost is Python _docker_host.
func dockerHost(value any, field string) (string, error) {
	s, ok := value.(string)
	if !ok || !strings.HasPrefix(s, "unix:///") {
		return "", fail(field, "only an explicit local unix:/// Docker socket is supported")
	}
	text := s[len("unix://"):]
	if _, err := pathText(text, field); err != nil {
		return "", err
	}
	if strings.ContainsAny(text, "?#") || text == "/" {
		return "", fail(field, "expected a local Unix socket path")
	}
	return "unix://" + pyPath(text), nil
}
