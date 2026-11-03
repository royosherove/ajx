// Package util holds the shared helpers ported from lib/ajx/util.py plus the
// dynamic-value accessors the Go port uses in place of Python dicts.
//
// Records such as the trial spec, state.json, run.json and normalized
// telemetry stay JSON-shaped (M, []any, string, float64/int64, bool, nil) so
// their files remain interchangeable with the Python implementation. Use the
// accessors below instead of raw type assertions: they accept both TOML
// (int64) and JSON (float64) numbers and never panic on a missing key.
package util

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// M is a JSON-shaped object (a Python dict).
type M = map[string]any

// Get walks nested maps: Get(spec, "trial", "id"). Missing keys yield nil.
func Get(v any, path ...string) any {
	cur := v
	for _, key := range path {
		m, ok := cur.(M)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// Map returns v as M, or nil when it is not an object.
func Map(v any) M {
	if m, ok := v.(M); ok {
		return m
	}
	return nil
}

// MapOr returns v as M, or a new empty M when it is not an object.
func MapOr(v any) M {
	if m, ok := v.(M); ok && m != nil {
		return m
	}
	return M{}
}

// List returns v as []any. Typed slices from TOML/JSON decoding are converted.
func List(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []M:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	}
	return nil
}

// Maps returns the object elements of a list (non-objects are skipped).
func Maps(v any) []M {
	var out []M
	for _, x := range List(v) {
		if m, ok := x.(M); ok {
			out = append(out, m)
		}
	}
	return out
}

// Str is Python's str() for scalars; nil becomes "".
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		// Python str(float) is repr(float), except for the special values.
		switch {
		case math.IsInf(t, 1):
			return "inf"
		case math.IsInf(t, -1):
			return "-inf"
		case math.IsNaN(t):
			return "nan"
		}
		return PyFloat(t)
	case int64, int, int32:
		return fmt.Sprint(t)
	}
	return fmt.Sprint(v)
}

// S returns v when it is a string, else "" (no conversion).
func S(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// Strings returns the string elements of a list; a lone string becomes [s].
func Strings(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
	var out []string
	for _, x := range List(v) {
		out = append(out, Str(x))
	}
	return out
}

// Num returns a numeric value as float64 and whether v was a number.
func Num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case uint64:
		return float64(t), true
	}
	return 0, false
}

// Int returns a number truncated to int, or def when v is not a number.
func Int(v any, def int) int {
	if f, ok := Num(v); ok {
		return int(f)
	}
	return def
}

// IsNum reports whether v is a JSON/TOML number (bools are not numbers).
func IsNum(v any) bool {
	_, ok := Num(v)
	return ok
}

// Truthy is Python truthiness.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case M:
		return len(t) > 0
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case []M:
		return len(t) > 0
	}
	if f, ok := Num(v); ok {
		return f != 0
	}
	return true
}

// Or returns the first truthy value (Python `a or b or c`), else the last.
func Or(vals ...any) any {
	for _, v := range vals {
		if Truthy(v) {
			return v
		}
	}
	if len(vals) == 0 {
		return nil
	}
	return vals[len(vals)-1]
}

// OrStr returns the first non-empty string.
func OrStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SortedKeys returns the keys of m in sorted order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Copy returns a shallow copy of m.
func Copy(m M) M {
	out := make(M, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// DeepCopy copies nested maps and lists (scalars are shared).
func DeepCopy(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = DeepCopy(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = DeepCopy(x)
		}
		return out
	}
	return v
}

// Normalize converts decoder output (typed slices, map[string]interface{}
// variants, int types) into the canonical JSON shape used across the port.
func Normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = Normalize(x)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Normalize(x)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	case int:
		return int64(t)
	case int32:
		return int64(t)
	}
	return v
}

// StrMap converts an object of scalars to map[string]string (Python str()).
func StrMap(v any) map[string]string {
	out := map[string]string{}
	for k, x := range Map(v) {
		out[k] = Str(x)
	}
	return out
}

// AnyMap converts map[string]string to M.
func AnyMap(m map[string]string) M {
	out := make(M, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Contains reports whether list holds s.
func Contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Clip truncates s to at most n characters (runes), like Python s[:n].
func Clip(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Tail keeps the last n characters (runes), like Python s[-n:].
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// RuneLen is len() of a Python str.
func RuneLen(s string) int { return len([]rune(s)) }

// OneLine collapses whitespace and clips to limit with a "..." suffix.
func OneLine(text string, limit int) string {
	t := strings.Join(strings.Fields(text), " ")
	if RuneLen(t) <= limit {
		return t
	}
	return Clip(t, limit-3) + "..."
}
