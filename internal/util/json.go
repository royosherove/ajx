package util

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DecodeJSON parses JSON into the canonical shape (M, []any, float64, ...).
// Integer literals decode as int64 (float64 when they overflow) and every
// other number as float64, so Python's int/float distinction survives a
// read-modify-write cycle.
func DecodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: extra data after the value")
	}
	return exactNumbers(v), nil
}

func exactNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = exactNumbers(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = exactNumbers(x)
		}
		return t
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	return v
}

// ReadJSON reads a JSON file. It returns def when the file does not exist and
// an error when it exists but cannot be read or parsed (Python read_json).
func ReadJSON(path string, def any) (any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	return DecodeJSON(data)
}

// ReadMap reads a JSON object file, returning an empty M when the file is
// missing, unreadable, malformed or not an object. For views that must not
// fail on a half-written record; use ReadJSON where corruption must surface.
func ReadMap(path string) M {
	v, err := ReadJSON(path, nil)
	if err != nil {
		return M{}
	}
	return MapOr(v)
}

// ReadMapStrict reads a JSON object file; missing -> empty M, malformed -> error.
func ReadMapStrict(path string) (M, error) {
	v, err := ReadJSON(path, nil)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return M{}, nil
	}
	m, ok := v.(M)
	if !ok {
		return nil, errors.New(path + ": not a JSON object")
	}
	return m, nil
}

// MarshalIndent is json.dumps(data, indent=2, ensure_ascii=False) plus "\n".
// Floats keep Python's spelling (2.0 stays 2.0); object keys are sorted.
func MarshalIndent(data any) ([]byte, error) {
	v, err := canonical(data)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	pyIndent(&b, v, 0)
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

// canonical converts arbitrary Go values (structs, typed maps and slices) to
// the JSON shape; already-canonical values pass through unchanged.
func canonical(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, string, float64, float32, int64, int, int32, uint64:
		return v, nil
	case M:
		out := make(M, len(t))
		for k, x := range t {
			c, err := canonical(x)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			c, err := canonical(x)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case map[string]string:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = x
		}
		return out, nil
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out, nil
	case []M:
		out := make([]any, len(t))
		for i, x := range t {
			c, err := canonical(x)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return DecodeJSON(raw)
}

func pyIndent(b *strings.Builder, v any, depth int) {
	pad := func(d int) {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", d))
	}
	switch t := v.(type) {
	case M:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range SortedKeys(t) {
			if i > 0 {
				b.WriteByte(',')
			}
			pad(depth + 1)
			b.WriteString(PyQuote(k))
			b.WriteString(": ")
			pyIndent(b, t[k], depth+1)
		}
		pad(depth)
		b.WriteByte('}')
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			pad(depth + 1)
			pyIndent(b, x, depth+1)
		}
		pad(depth)
		b.WriteByte(']')
	default:
		pyJSON(b, v)
	}
}

// WriteJSON atomically writes data as indented JSON (parents created).
func WriteJSON(path string, data any) error {
	out, err := MarshalIndent(data)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, out, 0o644)
}

// WriteFileAtomic writes via a sibling temporary file and rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSONL reads a JSON-lines file; unparsable lines become {"_unparsed": line}.
// A missing file yields nil.
func ReadJSONL(path string) []any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<30)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := DecodeJSON([]byte(line))
		if err != nil {
			rows = append(rows, M{"_unparsed": line})
			continue
		}
		rows = append(rows, v)
	}
	return rows
}

// AppendJSONL appends one compact JSON record and a newline.
func AppendJSONL(path string, rec any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(Compact(rec) + "\n")
	return err
}

// Compact is json.dumps(v, separators=(",", ":"), ensure_ascii=False)-like
// compact JSON (Go key order: sorted).
func Compact(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// PyJSON is Python's json.dumps(v, ensure_ascii=False) with its default
// separators (", " and ": "). Object keys are sorted (Go maps are unordered).
func PyJSON(v any) string {
	var b strings.Builder
	pyJSON(&b, v)
	return b.String()
}

func pyJSON(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(PyQuote(t))
	case float64:
		b.WriteString(PyFloat(t))
	case float32:
		b.WriteString(PyFloat(float64(t)))
	case int64, int, int32, uint64:
		b.WriteString(Str(t))
	case json.Number:
		b.WriteString(t.String())
	case M:
		keys := SortedKeys(t)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(PyQuote(k))
			b.WriteString(": ")
			pyJSON(b, t[k])
		}
		b.WriteByte('}')
	case map[string]string:
		keys := SortedKeys(t)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(PyQuote(k))
			b.WriteString(": ")
			b.WriteString(PyQuote(t[k]))
		}
		b.WriteByte('}')
	default:
		if l := List(v); l != nil || v == nil {
			b.WriteByte('[')
			for i, x := range l {
				if i > 0 {
					b.WriteString(", ")
				}
				pyJSON(b, x)
			}
			b.WriteByte(']')
			return
		}
		raw, err := json.Marshal(v)
		if err != nil {
			b.WriteString("null")
			return
		}
		if d, err := DecodeJSON(raw); err == nil {
			pyJSON(b, d)
			return
		}
		b.Write(raw)
	}
}

// PyFloat formats a float like Python's repr (1.0, 0.25, 1e-07, inf as Infinity).
func PyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	// Python's repr uses the shortest round-tripping digits and switches to
	// exponent notation only when the decimal exponent is < -4 or >= 16.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(e, "e")
	n, _ := strconv.Atoi(exp)
	if n >= -4 && n < 16 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	sign := exp[:1]
	digits := strings.TrimLeft(exp[1:], "0")
	if len(digits) < 2 {
		digits = strings.Repeat("0", 2-len(digits)) + digits
	}
	return mant + "e" + sign + digits
}

// PyQuote is a JSON string literal as Python's json.dumps(ensure_ascii=False) writes it.
func PyQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == utf8.RuneError {
				if r == utf8.RuneError {
					b.WriteString(`�`)
				} else {
					b.WriteString(`\u00`)
					b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
					b.WriteString(strconv.FormatInt(int64(r)&0xf, 16))
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// SortedMapKeys returns the keys of an M sorted (convenience alias).
func SortedMapKeys(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
