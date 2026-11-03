// Package res gives access to the skill's resources: embedded in the binary by
// default, or read from a checkout when AJX_SKILL_DIR names a skills/ajx
// directory (useful while editing console assets or prompts without rebuilding).
package res

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ajx "github.com/royosherove/ajx"
)

// FingerprintParts are the parts of the skill that define its behavior.
var FingerprintParts = []string{"SKILL.md", "prompts", "schemas", "references", "assets"}

// Dir returns the on-disk skill directory when AJX_SKILL_DIR is set, else "".
func Dir() string {
	if d := os.Getenv("AJX_SKILL_DIR"); d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			return abs
		}
		return d
	}
	return ""
}

// FS is the skill tree rooted at skills/ajx (SKILL.md, prompts/, schemas/, ...).
func FS() fs.FS {
	if d := Dir(); d != "" {
		return os.DirFS(d)
	}
	sub, err := fs.Sub(ajx.Skill, "skills/ajx")
	if err != nil {
		panic(err)
	}
	return sub
}

// Root is a human-readable location of the skill resources, for evidence records.
func Root() string {
	if d := Dir(); d != "" {
		return d
	}
	return "embedded:skills/ajx"
}

// ReadFile reads a skill-relative path such as "prompts/narrate.md".
func ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(FS(), filepath.ToSlash(name))
}

// MustText reads a skill-relative file as text and panics if it is missing:
// for resources that are compiled into the binary.
func MustText(name string) string {
	b, err := ReadFile(name)
	if err != nil {
		panic("ajx resource missing: " + name + ": " + err.Error())
	}
	return string(b)
}

// CopyDir copies the skill-relative directory src into dest (created as needed).
// skip returns true for slash-separated relative paths (files or dirs) to omit.
func CopyDir(src, dest string, skip func(rel string, isDir bool) bool) error {
	fsys := FS()
	return fs.WalkDir(fsys, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		if rel != "" && skip != nil && skip(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info, err := d.Info(); err == nil && info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
}

// Fingerprint is a content hash of the behavior-defining resources plus the
// implementation, recorded as ajx_skill_revision ("sha256:" + 16 hex chars).
func Fingerprint() string {
	fsys := FS()
	h := sha256.New()
	h.Write([]byte("ajx-go " + ajx.Version + "\n"))
	for _, part := range FingerprintParts {
		var files []string
		_ = fs.WalkDir(fsys, part, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !strings.Contains(p, "__pycache__") {
				files = append(files, p)
			}
			return nil
		})
		sort.Strings(files)
		for _, p := range files {
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				continue
			}
			h.Write([]byte(p))
			h.Write(data)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}
