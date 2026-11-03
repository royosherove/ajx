package util

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const tsLayout = "2006-01-02T15:04:05.000Z"

// Now is the current UTC time as ISO-8601 with milliseconds and a Z suffix.
func Now() string { return time.Now().UTC().Format(tsLayout) }

// FormatTS formats t like Now.
func FormatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

var tsLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
}

// ParseTS parses ISO-8601 timestamps the way Python's datetime.fromisoformat
// does after replacing Z with +00:00. Values without an offset are local time.
func ParseTS(value string) (time.Time, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for i, layout := range tsLayouts {
		var t time.Time
		var err error
		if strings.Contains(layout, "Z07") || i == 0 {
			t, err = time.Parse(layout, v)
		} else {
			t, err = time.ParseInLocation(layout, v, time.Local)
		}
		if err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid isoformat string: %q", value)
}

// SecondsBetween returns b-a in seconds rounded to 3 decimals; nil when either
// is empty or unparsable.
func SecondsBetween(a, b string) *float64 {
	if a == "" || b == "" {
		return nil
	}
	ta, err1 := ParseTS(a)
	tb, err2 := ParseTS(b)
	if err1 != nil || err2 != nil {
		return nil
	}
	v := Round(tb.Sub(ta).Seconds(), 3)
	return &v
}

// Round rounds half to even at the given number of decimals (Python round()).
func Round(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.RoundToEven(f*p) / p
}

// Sha256Bytes is the hex SHA-256 of data.
func Sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Sha256File is the hex SHA-256 of a file's bytes.
func Sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Sha256Bytes(data), nil
}

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// Fill replaces {name} placeholders present in values; every other brace is
// left untouched (unlike fmt or str.format).
func Fill(template string, values map[string]string) string {
	return placeholder.ReplaceAllStringFunc(template, func(m string) string {
		key := m[1 : len(m)-1]
		if v, ok := values[key]; ok {
			return v
		}
		return m
	})
}

// FillAll fills every item.
func FillAll(items []string, values map[string]string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = Fill(it, values)
	}
	return out
}

// Environ returns the caller's environment as a map.
func Environ() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// Dropped reports whether key matches drop (exact names, or prefixes ending in '*').
func Dropped(key string, drop []string) bool {
	for _, d := range drop {
		if strings.HasSuffix(d, "*") {
			if strings.HasPrefix(key, d[:len(d)-1]) {
				return true
			}
		} else if key == d {
			return true
		}
	}
	return false
}

// ChildEnv inherits the caller's environment minus `drop` (names or 'PREFIX*'),
// then applies extra (Python util.child_env).
func ChildEnv(extra map[string]string, drop []string) map[string]string {
	env := map[string]string{}
	for k, v := range Environ() {
		if !Dropped(k, drop) {
			env[k] = v
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// EnvList converts an env map to the KEY=VALUE list exec.Cmd expects, sorted.
func EnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range SortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// MergeEnv returns a new map: base overlaid by each of overlays in order.
func MergeEnv(base map[string]string, overlays ...map[string]string) map[string]string {
	out := make(map[string]string, len(base))
	for k, v := range base {
		out[k] = v
	}
	for _, o := range overlays {
		for k, v := range o {
			out[k] = v
		}
	}
	return out
}

// Uniq returns the sorted distinct strings.
func Uniq(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	sort.Strings(out)
	return out
}

// Home is the user's home directory ("" when unknown).
func Home() string {
	h, _ := os.UserHomeDir()
	return h
}

// ShortPath shows a path relative to the working directory when inside it,
// with ~ for the home directory otherwise.
func ShortPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, ok := strings.CutPrefix(p, wd+string(os.PathSeparator)); ok {
			return rel
		}
		if p == wd {
			return "."
		}
	}
	if h := Home(); h != "" {
		if rel, ok := strings.CutPrefix(p, h+string(os.PathSeparator)); ok {
			return "~/" + rel
		}
	}
	return p
}
