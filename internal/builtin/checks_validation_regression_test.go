package builtin

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
)

func TestFileCountRejectsInvalidThresholdBeforeVerification(t *testing.T) {
	dir := t.TempDir()
	ctx := &plugin.Ctx{Workspace: dir}
	for _, v := range []any{0, -1, int64(-5), 1.5, true, false, nil, "0", "-1", "1junk", "1.5", M{}, math.Inf(1)} {
		if _, err := FileExistsCheck(M{"path": "missing.txt", "min_count": v}, ctx); err == nil {
			t.Errorf("invalid threshold %#v accepted", v)
		}
	}
	for _, v := range []any{1, int64(1), 1.0, "1"} {
		result, err := FileExistsCheck(M{"path": "missing.txt", "min_count": v}, ctx)
		if err != nil || result["passed"] != false {
			t.Fatalf("missing file verified for %#v: %v %v", v, result, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := FileExistsCheck(M{"path": "result.txt", "min_count": "1"}, ctx)
	if err != nil || result["passed"] != true {
		t.Fatalf("valid integer string: %v %v", result, err)
	}
}

func TestHTTPCheckURLSyntaxWithoutNetwork(t *testing.T) {
	for _, raw := range []string{"", "http:", "http://", "https://", "file:///tmp/result", "http://host:bad", " http://host", "http://host/\n"} {
		if err := ValidateHTTPURL(raw); err == nil {
			t.Errorf("invalid URL accepted: %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080/", "https://example.invalid/path", "${CHECK_URL}", "http://${HOST}:${PORT}/health"} {
		if err := ValidateHTTPURL(raw); err != nil {
			t.Errorf("valid URL/template rejected: %q: %v", raw, err)
		}
	}
}
