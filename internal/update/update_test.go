package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLatestUsesPinnedAnonymousAssetsAndChecksums(t *testing.T) {
	release := Release{1, "1.2.3", strings.Repeat("a", 40), strings.Repeat("b", 64)}
	payload, _ := json.Marshal(release)
	version := []byte("v1.2.3\n")
	assets := map[string][]byte{
		"/latest/download/VERSION":      version,
		"/download/v1.2.3/RELEASE.json": payload,
		"/download/v1.2.3/SHA256SUMS": []byte(fmt.Sprintf("%x  VERSION\n%x  RELEASE.json\n",
			sha256.Sum256(version), sha256.Sum256(payload))),
	}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("release request carried credentials")
		}
		requests = append(requests, r.URL.Path)
		if value, ok := assets[r.URL.Path]; ok {
			w.Write(value)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	got, err := Latest(server.Client(), server.URL)
	if err != nil || got != release || len(requests) != 3 {
		t.Fatalf("release %v, requests %v, error %v", got, requests, err)
	}
	assets["/download/v1.2.3/RELEASE.json"] = []byte(`{"schema":1}`)
	if _, err := Latest(server.Client(), server.URL); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("accepted corrupt metadata: %v", err)
	}
}

func TestInvalidAndMissingReleaseAssetsFailWithoutFallback(t *testing.T) {
	for _, version := range []string{"../../main", "v1.2.3-rc1", "v1.2.3/other"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, version)
		}))
		_, err := Latest(server.Client(), server.URL)
		server.Close()
		if err == nil {
			t.Fatalf("accepted %q", version)
		}
	}
	data := []byte("ok")
	sum := fmt.Sprintf("%x  VERSION\n", sha256.Sum256(data))
	if verify([]byte(sum+sum), data, "VERSION") == nil {
		t.Fatal("accepted duplicate checksum")
	}
	if verify(nil, data, "VERSION") == nil {
		t.Fatal("accepted missing checksum")
	}
}

func skill(t *testing.T, home string) string {
	t.Helper()
	root := filepath.Join(home, ".local/share/ajx/current/skill")
	for _, part := range []string{"prompts", "schemas", "references", "assets", "examples"} {
		if err := os.MkdirAll(filepath.Join(root, part), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("Use AJX"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInspectSeparatesCLIAndSkillAndNeverDowngrades(t *testing.T) {
	home := t.TempDir()
	path := skill(t, home)
	hash, err := SkillHash(os.DirFS(path))
	if err != nil {
		t.Fatal(err)
	}
	release := Release{1, "1.2.3", strings.Repeat("a", 40), hash}
	for _, tc := range []struct {
		version, build, skill string
		cli, changed, ahead   bool
	}{
		{"1.2.2", "old", hash, true, false, false},
		{"1.2.3", release.Commit, hash, false, false, false},
		{"1.2.3", release.Commit, strings.Repeat("b", 64), false, true, false},
		{"1.2.3", "other", hash, true, false, false},
		{"1.3.0", "newer", strings.Repeat("b", 64), false, false, true},
	} {
		release.SkillHash = tc.skill
		got, err := Inspect(release, tc.version, tc.build, home)
		if err != nil || got.CLIAvailable != tc.cli || got.SkillAvailable != tc.changed || got.Ahead != tc.ahead {
			t.Fatalf("%+v: %+v, %v", tc, got, err)
		}
	}
	got, err := Inspect(release, "1.2.3", release.Commit, t.TempDir())
	if err != nil || got.SkillInstalled || !got.SkillAvailable {
		t.Fatalf("missing skill: %+v %v", got, err)
	}
}

func TestSkillHashIgnoresCLIAndDetectsInstructionChanges(t *testing.T) {
	tree := fstest.MapFS{
		"SKILL.md": {Data: []byte("Use AJX")},
		"prompts":  {Mode: os.ModeDir}, "schemas": {Mode: os.ModeDir},
		"references": {Mode: os.ModeDir}, "assets": {Mode: os.ModeDir},
		"examples": {Mode: os.ModeDir},
		"bin/ajx":  {Data: []byte("old binary")},
	}
	first, err := SkillHash(tree)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	fileHash := sha256.Sum256([]byte("Use AJX"))
	digest.Write([]byte("SKILL.md\x00"))
	digest.Write(fileHash[:])
	if first != hex.EncodeToString(digest.Sum(nil)) {
		t.Fatal("unexpected hash format")
	}
	tree["bin/ajx"].Data = []byte("new binary")
	second, _ := SkillHash(tree)
	if first != second {
		t.Fatal("CLI changes affected skill content hash")
	}
	tree["SKILL.md"].Data = []byte("New instructions")
	second, _ = SkillHash(tree)
	if first == second {
		t.Fatal("missed changed skill")
	}
}
