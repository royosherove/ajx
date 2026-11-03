package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeadlessSchemaRejectsMalformedConsumedFields(t *testing.T) {
	tests := []struct{ name, extra, field string }{
		{"missing HTTP URL", "[[verify]]\ntype='http'", "url"},
		{"numeric HTTP URL", "[[verify]]\ntype='http'\nurl=42", "url"},
		{"hostless HTTP URL", "[[verify]]\ntype='http'\nurl='http://'", "url"},
		{"numeric shell", "[[verify]]\nrun=42", "run"},
		{"array shell", "[[verify]]\nrun=['echo','hello']", "run"},
		{"blank shell", "[[verify]]\nrun='   '", "run"},
		{"file regex", "[[verify]]\ntype='file_exists'\npath='result'\ncontains='['", "contains"},
		{"shell regex", "[[verify]]\nrun='true'\nexpect_stdout='['", "expect_stdout"},
		{"HTTP regex", "[[verify]]\ntype='http'\nurl='http://example.invalid'\nexpect_body='['", "expect_body"},
		{"zero timeout", "[[verify]]\nrun='true'\ntimeout=0", "timeout"},
		{"negative timeout", "[[preflight]]\nrun='true'\ntimeout=-1", "timeout"},
		{"zero count", "[[verify]]\ntype='file_exists'\npath='result'\nmin_count=0", "min_count"},
		{"negative count", "[[verify]]\ntype='file_exists'\npath='result'\nmin_count=-1", "min_count"},
		{"fraction count", "[[verify]]\ntype='file_exists'\npath='result'\nmin_count=1.5", "min_count"},
		{"bool count", "[[verify]]\ntype='file_exists'\npath='result'\nmin_count=true", "min_count"},
		{"auth env array", "[auth.test]\ntype='env'\nenv=['a']", "auth.test.env"},
		{"auth command string", "[auth.test]\ntype='env'\nidentity_cmd='whoami'", "identity_cmd"},
		{"auth command blank", "[auth.test]\ntype='env'\nidentity_cmd=[' ']", "identity_cmd"},
		{"cell env number", "[[cells]]\nid='bad'\nharness='codex'\nenv=1", "env"},
		{"cell model table", "[[cells]]\nid='bad'\nharness='codex'\nmodel={bad='type'}", "model"},
		{"adapter command element", "[adapters.test]\nexecute=['sh',1]", "execute"},
		{"adapter version string", "[adapters.test]\nexecute=['sh']\nversion='sh'", "version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(write(t, t.TempDir(), test.extra), LoadOpts{SkipReadiness: true})
			var configErr *Error
			if !errors.As(err, &configErr) || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("want configuration error naming %s, got %v", test.field, err)
			}
		})
	}
}

func TestHeadlessDecodeErrorsRemainTypedConfigurationErrors(t *testing.T) {
	for name, text := range map[string]string{
		"syntax": "[trial\n", "table type": "trial=42\n",
		"duplicate": "[trial]\nid='a'\n[trial]\nid='b'\n", "UTF8": "\xff",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trial.toml")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path, LoadOpts{})
			if !IsError(err) {
				t.Fatalf("want configuration error, got %T: %v", err, err)
			}
		})
	}
	_, err := DecodeTOML("[trial\n")
	var syntax *DecodeError
	if !errors.As(err, &syntax) || syntax.Line == 0 || !IsError(err) {
		t.Fatalf("lost syntax error location or compatibility type: %v", err)
	}
}

func TestHeadlessSchemaPreservesSupportedScalarCoercionsAndExtensions(t *testing.T) {
	path := write(t, t.TempDir(), `
[auth.test]
type='env'
env={TEXT='value', NUMBER=42, BOOLEAN=true}
identity_cmd=[]
[adapters.test]
execute=['sh','-c','true']
env={NUMBER=42}
extension_setting={private=true}
[[verify]]
type='file_exists'
path='result'
min_count='1'
timeout='5'
[[verify]]
type='http'
url='http://${HOST}:${PORT}/health'
[[cells]]
id='custom'
harness='test'
auth='test'
env={NUMBER=42, BOOLEAN=false}
`)
	if _, err := Load(path, LoadOpts{SkipReadiness: true}); err != nil {
		t.Fatal(err)
	}
}
