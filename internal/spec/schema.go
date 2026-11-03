package spec

import (
	"math"
	"regexp"
	"strings"

	"github.com/royosherove/ajx/internal/builtin"
)

// Validate only fields consumed by the native implementation. Unknown keys
// remain available to extensions; scalar environment values retain coercion.
func validateStrings(m M, field string, keys ...string) error {
	for _, key := range keys {
		v, present := m[key]
		if !present || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok || strings.ContainsRune(s, '\x00') {
			return Errorf("%s.%s must be a string without NUL characters", field, key)
		}
	}
	return nil
}

func validateStringList(m M, key, field string) error {
	v, present := m[key]
	if !present {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return Errorf("%s.%s must be a list of strings", field, key)
	}
	for _, value := range list {
		s, ok := value.(string)
		if !ok || strings.ContainsRune(s, '\x00') {
			return Errorf("%s.%s must be a list of strings without NUL characters", field, key)
		}
	}
	return nil
}

func validateEnv(m M, key, field string) error {
	v, present := m[key]
	if !present {
		return nil
	}
	env, ok := v.(M)
	if !ok {
		return Errorf("%s.%s must be a table of scalar environment values", field, key)
	}
	for name, value := range env {
		valid := true
		switch x := value.(type) {
		case string:
			valid = !strings.ContainsRune(x, '\x00')
		case bool, int, int64:
		case float64:
			valid = !math.IsNaN(x) && !math.IsInf(x, 0)
		default:
			valid = false
		}
		if name == "" || strings.ContainsAny(name, "=\x00") || !valid {
			return Errorf("%s.%s.%s must be a scalar environment value with a valid name", field, key, name)
		}
	}
	return nil
}

func validateAuthFields(m M, field string) error {
	if err := validateEnv(m, "env", field); err != nil {
		return err
	}
	for _, key := range []string{"args", "unset", "required_env", "model_env", "identity_cmd"} {
		if err := validateStringList(m, key, field); err != nil {
			return err
		}
	}
	if cmd, ok := m["identity_cmd"].([]any); ok && len(cmd) > 0 && strings.TrimSpace(cmd[0].(string)) == "" {
		return Errorf("%s.identity_cmd must start with an executable", field)
	}
	return nil
}

func validateAdapterFields(m M, field string) error {
	for _, key := range []string{"execute", "resume", "report", "version", "model_args", "effort_args",
		"clean_args", "no_tools_args", "report_args", "strip_env"} {
		if err := validateStringList(m, key, field); err != nil {
			return err
		}
	}
	if cmd, ok := m["execute"].([]any); ok && len(cmd) > 0 && strings.TrimSpace(cmd[0].(string)) == "" {
		return Errorf("%s.execute must start with an executable", field)
	}
	for _, key := range []string{"env", "clean_env"} {
		if err := validateEnv(m, key, field); err != nil {
			return err
		}
	}
	return nil
}

func validateCheckPatterns(m M, field string) error {
	keys := []string{}
	switch m["type"] {
	case "shell":
		keys = []string{"expect_stdout"}
	case "file_exists":
		keys = []string{"contains"}
	case "http":
		keys = []string{"expect_body"}
	}
	for _, key := range keys {
		if err := validateStrings(m, field, key); err != nil {
			return err
		}
		if pattern, ok := m[key].(string); ok {
			if _, err := regexp.Compile(pattern); err != nil {
				return Errorf("%s.%s is not a valid regular expression: %s", field, key, err)
			}
		}
	}
	for _, key := range []string{"expect_exit", "expect_status"} {
		if v, present := m[key]; present {
			if _, ok := pyInt(v); !ok {
				return Errorf("%s.%s must be an integer", field, key)
			}
		}
	}
	return nil
}

// ValidateHTTPURL is shared with the initializer so the same syntax is
// accepted in --check and a hand-written trial.
func ValidateHTTPURL(raw string) error {
	return builtin.ValidateHTTPURL(raw)
}
