// Package update checks public release assets without GitHub credentials.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const Releases = "https://github.com/royosherove/ajx/releases"

var versionRE = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)
var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Release struct {
	Schema    int    `json:"schema"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	SkillHash string `json:"skill_sha256"`
}

type Status struct {
	CurrentVersion string  `json:"current_version"`
	CurrentBuild   string  `json:"current_build"`
	Release        Release `json:"release"`
	CLIAvailable   bool    `json:"cli_update_available"`
	SkillAvailable bool    `json:"skill_update_available"`
	SkillInstalled bool    `json:"skill_installed"`
	CurrentSkill   string  `json:"current_skill_sha256"`
	Ahead          bool    `json:"newer_than_release"`
}

func Client() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != "https" {
				return fmt.Errorf("refusing an insecure or excessive release redirect")
			}
			return nil
		},
	}
}

// SkillHash covers the instructions, reports, schemas and examples independently
// of the CLI version, so a CLI-only update can leave an older skill in place.
func SkillHash(tree fs.FS) (string, error) {
	var paths []string
	for _, part := range []string{"SKILL.md", "prompts", "schemas", "references", "assets", "examples"} {
		err := fs.WalkDir(tree, part, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("unexpected symlink in managed skill: %s", path)
			}
			if !entry.IsDir() && !strings.Contains(path, "__pycache__") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		data, err := fs.ReadFile(tree, path)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(data)
		hash.Write([]byte(path + "\x00"))
		hash.Write(digest[:])
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fetch(client *http.Client, url string) ([]byte, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("could not check the AJX release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release asset unavailable (HTTP %d): %s", response.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("could not read release metadata safely")
	}
	return data, nil
}

func verify(sums, data []byte, name string) error {
	found := 0
	digest := sha256.Sum256(data)
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			found++
			if len(fields) != 2 || fields[0] != hex.EncodeToString(digest[:]) {
				return fmt.Errorf("release checksum mismatch for %s; nothing installed", name)
			}
		}
	}
	if found != 1 {
		return fmt.Errorf("missing or duplicate release checksum for %s", name)
	}
	return nil
}

func Latest(client *http.Client, base string) (Release, error) {
	var release Release
	version, err := fetch(client, base+"/latest/download/VERSION")
	if err != nil {
		return release, err
	}
	tag := strings.TrimSpace(string(version))
	if !strings.HasPrefix(tag, "v") || !versionRE.MatchString(tag) {
		return release, fmt.Errorf("invalid stable release version")
	}
	pinned := base + "/download/" + tag
	sums, err := fetch(client, pinned+"/SHA256SUMS")
	if err != nil {
		return release, err
	}
	if err := verify(sums, version, "VERSION"); err != nil {
		return release, err
	}
	data, err := fetch(client, pinned+"/RELEASE.json")
	if err != nil {
		return release, err
	}
	if err := verify(sums, data, "RELEASE.json"); err != nil {
		return release, err
	}
	if json.Unmarshal(data, &release) != nil || release.Schema != 1 ||
		release.Version != strings.TrimPrefix(tag, "v") ||
		!hashRE.MatchString(release.SkillHash) || !commitRE.MatchString(release.Commit) {
		return Release{}, fmt.Errorf("invalid release metadata; nothing installed")
	}
	return release, nil
}

func compareVersions(a, b string) (int, error) {
	left, right := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if left == nil || right == nil {
		return 0, fmt.Errorf("cannot compare non-stable AJX versions")
	}
	for i := 1; i <= 3; i++ {
		x, ex := strconv.ParseUint(left[i], 10, 32)
		y, ey := strconv.ParseUint(right[i], 10, 32)
		if ex != nil || ey != nil {
			return 0, fmt.Errorf("invalid AJX version number")
		}
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
	}
	return 0, nil
}

func Inspect(release Release, current, build, home string) (Status, error) {
	state := Status{CurrentVersion: current, CurrentBuild: build, Release: release}
	comparison, err := compareVersions(current, release.Version)
	if err != nil {
		return state, err
	}
	state.Ahead = comparison > 0
	state.CLIAvailable = comparison < 0 || (comparison == 0 && build != release.Commit)
	skill := filepath.Join(home, ".local", "share", "ajx", "current", "skill")
	if _, err := os.Stat(skill); err == nil {
		state.SkillInstalled = true
		state.CurrentSkill, err = SkillHash(os.DirFS(skill))
		if err != nil {
			return state, fmt.Errorf("cannot inspect the installed skill: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return state, err
	}
	state.SkillAvailable = !state.Ahead && state.CurrentSkill != release.SkillHash
	return state, nil
}
