package planedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

// Dict is a Python dict with string keys: it remembers insertion order. The
// plan model, the trial file's tables and every table the editor writes are
// Dicts, so rewritten sections list their keys in the order Python writes them.
type Dict struct {
	keys []string
	vals map[string]any
}

// NewDict builds a Dict from key, value pairs.
func NewDict(pairs ...any) *Dict {
	d := &Dict{vals: make(map[string]any, len(pairs)/2)}
	for i := 0; i+1 < len(pairs); i += 2 {
		d.Set(pairs[i].(string), pairs[i+1])
	}
	return d
}

// Len is len(d).
func (d *Dict) Len() int {
	if d == nil {
		return 0
	}
	return len(d.keys)
}

// Keys are the keys in insertion order.
func (d *Dict) Keys() []string {
	if d == nil {
		return nil
	}
	return append([]string(nil), d.keys...)
}

// Has is `k in d`.
func (d *Dict) Has(k string) bool {
	if d == nil {
		return false
	}
	_, ok := d.vals[k]
	return ok
}

// Get is d.get(k).
func (d *Dict) Get(k string) any {
	if d == nil {
		return nil
	}
	return d.vals[k]
}

// Lookup is the value and whether k is present.
func (d *Dict) Lookup(k string) (any, bool) {
	if d == nil {
		return nil, false
	}
	v, ok := d.vals[k]
	return v, ok
}

// Set is d[k] = v: a new key goes last, an existing one keeps its place.
func (d *Dict) Set(k string, v any) {
	if d.vals == nil {
		d.vals = map[string]any{}
	}
	if _, ok := d.vals[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.vals[k] = v
}

// Delete is d.pop(k, None).
func (d *Dict) Delete(k string) {
	if !d.Has(k) {
		return
	}
	delete(d.vals, k)
	for i, x := range d.keys {
		if x == k {
			d.keys = append(d.keys[:i:i], d.keys[i+1:]...)
			break
		}
	}
}

// Copy is dict(d): a shallow copy.
func (d *Dict) Copy() *Dict {
	out := &Dict{keys: d.Keys(), vals: make(map[string]any, d.Len())}
	if d != nil {
		for k, v := range d.vals {
			out.vals[k] = v
		}
	}
	return out
}

// Update is d.update(o).
func (d *Dict) Update(o *Dict) {
	for _, k := range o.Keys() {
		d.Set(k, o.vals[k])
	}
}

// MarshalJSON writes the Dict in insertion order.
func (d *Dict) MarshalJSON() ([]byte, error) { return []byte(Dumps(d)), nil }

// DeepCopy is copy.deepcopy for model values.
func DeepCopy(v any) any {
	switch t := v.(type) {
	case *Dict:
		out := &Dict{keys: t.Keys(), vals: make(map[string]any, t.Len())}
		for k, x := range t.vals {
			out.vals[k] = DeepCopy(x)
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

// ToM converts Dicts (also inside lists) to util.M for JSON records; other
// values pass through.
func ToM(v any) any {
	switch t := v.(type) {
	case *Dict:
		out := make(util.M, t.Len())
		for k, x := range t.vals {
			out[k] = ToM(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = ToM(x)
		}
		return out
	}
	return v
}

// FromM converts util.M values (also inside lists) to Dicts with sorted keys.
func FromM(v any) any {
	switch t := v.(type) {
	case util.M:
		out := NewDict()
		for _, k := range util.SortedKeys(t) {
			out.Set(k, FromM(t[k]))
		}
		return out
	case *Dict:
		return t
	}
	if l := util.List(v); l != nil {
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = FromM(x)
		}
		return out
	}
	switch t := v.(type) {
	case int:
		return int64(t)
	case int32:
		return int64(t)
	}
	return v
}

// DecodeJSON is json.loads keeping every object's key order: objects become
// Dicts, integer literals int64 and other numbers float64.
func DecodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func() (any, error)
	walk = func() (any, error) {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '[' {
				out := []any{}
				for dec.More() {
					v, err := walk()
					if err != nil {
						return nil, err
					}
					out = append(out, v)
				}
				_, err := dec.Token()
				return out, err
			}
			d := NewDict()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := walk()
				if err != nil {
					return nil, err
				}
				d.Set(kt.(string), v)
			}
			_, err := dec.Token()
			return d, err
		case json.Number:
			s := t.String()
			if !strings.ContainsAny(s, ".eE") {
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					return n, nil
				}
			}
			f, _ := strconv.ParseFloat(s, 64)
			return f, nil
		}
		return tok, nil
	}
	v, err := walk()
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: extra data after the value")
	}
	return v, nil
}

// Dumps is json.dumps(v, ensure_ascii=False) keeping Dict key order.
func Dumps(v any) string {
	var b strings.Builder
	dumps(&b, v)
	return b.String()
}

func dumps(b *strings.Builder, v any) {
	switch t := v.(type) {
	case *Dict:
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(util.PyQuote(k))
			b.WriteString(": ")
			dumps(b, t.vals[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			dumps(b, x)
		}
		b.WriteByte(']')
	case time.Time:
		b.WriteString(util.PyQuote(pyStr(t)))
	default:
		b.WriteString(util.PyJSON(v))
	}
}

// ---------------------------------------------------------------- Python semantics

// Error is a plan the editor cannot write (Python PlanError).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// pyError is a Python exception the port raises where Python would
// (KeyError, TypeError, ValueError, AttributeError, IndexError).
type pyError struct{ typ, msg string }

func (e *pyError) Error() string { return e.msg }

func fail(format string, a ...any) { panic(&Error{Msg: fmt.Sprintf(format, a...)}) }

func raise(typ, format string, a ...any) { panic(&pyError{typ, fmt.Sprintf(format, a...)}) }

// catch runs fn and returns the PlanError or Python exception it raised.
func catch(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *Error:
				err = e
			case *pyError:
				err = e
			default:
				panic(r)
			}
		}
	}()
	fn()
	return nil
}

func typeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *Dict:
		return "dict"
	case time.Time:
		switch t.Location().String() {
		case "date-local":
			return "date"
		case "time-local":
			return "time"
		}
		return "datetime"
	}
	return fmt.Sprintf("%T", v)
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

// Equal is Python ==: numbers by value (True == 1 == 1.0), dicts regardless
// of key order, lists element by element.
func Equal(a, b any) bool {
	an, aok := num(a)
	bn, bok := num(b)
	if aok || bok {
		return aok && bok && an == bn
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case *Dict:
		y, ok := b.(*Dict)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for k, v := range x.vals {
			w, present := y.vals[k]
			if !present || !Equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case time.Time:
		y, ok := b.(time.Time)
		return ok && typeName(x) == typeName(y) && pyStr(x) == pyStr(y)
	}
	return false
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case *Dict:
		return t.Len() > 0
	}
	return true
}

// or is Python `a or b`.
func or(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// pyStr is Python str(v).
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case time.Time:
		frac := ""
		if us := t.Nanosecond() / 1000; us != 0 {
			frac = fmt.Sprintf(".%06d", us)
		}
		switch t.Location().String() {
		case "date-local":
			return t.Format("2006-01-02")
		case "time-local":
			return t.Format("15:04:05") + frac
		case "datetime-local":
			return t.Format("2006-01-02 15:04:05") + frac
		}
		return t.Format("2006-01-02 15:04:05") + frac + t.Format("-07:00")
	}
	return repr(v)
}

// repr is Python repr(v).
func repr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return pyfmt.FloatRepr(t)
	case string:
		return pyfmt.StrRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = repr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Dict:
		parts := make([]string, 0, t.Len())
		for _, k := range t.keys {
			parts = append(parts, pyfmt.StrRepr(k)+": "+repr(t.vals[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case time.Time:
		return "datetime." + typeName(t) + "(" + pyStr(t) + ")"
	}
	return fmt.Sprint(v)
}

// asDict is v when it is a dict, else nil.
func asDict(v any) *Dict {
	d, _ := v.(*Dict)
	return d
}

// get is v.get(k) (AttributeError when v is not a dict).
func get(v any, k string) any {
	d, ok := v.(*Dict)
	if !ok {
		raise("AttributeError", "'%s' object has no attribute 'get'", typeName(v))
	}
	return d.Get(k)
}

// getDef is v.get(k, def).
func getDef(v any, k string, def any) any {
	d, ok := v.(*Dict)
	if !ok {
		raise("AttributeError", "'%s' object has no attribute 'get'", typeName(v))
	}
	if x, present := d.Lookup(k); present {
		return x
	}
	return def
}

// hashKey is a dict key: str() for hashable values, TypeError for lists and dicts.
func hashKey(k any) (string, bool) {
	switch k.(type) {
	case []any, *Dict:
		raise("TypeError", "unhashable type: '%s'", typeName(k))
	case string:
		return k.(string), true
	}
	return "", false // a number, bool or None is never one of these string keys
}

// getAny is d.get(k) for a key of any type.
func getAny(v any, k any) any {
	d, ok := v.(*Dict)
	if !ok {
		raise("AttributeError", "'%s' object has no attribute 'get'", typeName(v))
	}
	if s, ok := hashKey(k); ok {
		return d.Get(s)
	}
	return nil
}

// item is v[k] for a string key.
func item(v any, k string) any {
	switch t := v.(type) {
	case *Dict:
		x, ok := t.Lookup(k)
		if !ok {
			raise("KeyError", "%s", pyfmt.StrRepr(k))
		}
		return x
	case []any:
		raise("TypeError", "list indices must be integers or slices, not str")
	case string:
		raise("TypeError", "string indices must be integers, not 'str'")
	}
	raise("TypeError", "'%s' object is not subscriptable", typeName(v))
	return nil
}

// has is `x in v`.
func has(v any, x any) bool {
	switch t := v.(type) {
	case *Dict:
		if s, ok := hashKey(x); ok {
			return t.Has(s)
		}
		return false
	case []any:
		for _, y := range t {
			if Equal(y, x) {
				return true
			}
		}
		return false
	case string:
		s, ok := x.(string)
		if !ok {
			raise("TypeError", "'in <string>' requires string as left operand, not %s", typeName(x))
		}
		return strings.Contains(t, s)
	}
	raise("TypeError", "argument of type '%s' is not iterable", typeName(v))
	return false
}

// iter is `for x in v`.
func iter(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case *Dict:
		out := make([]any, 0, t.Len())
		for _, k := range t.keys {
			out = append(out, k)
		}
		return out
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	}
	raise("TypeError", "'%s' object is not iterable", typeName(v))
	return nil
}

// orList is `for x in v or []`.
func orList(v any) []any {
	if !truthy(v) {
		return nil
	}
	return iter(v)
}

// orDict is `v or {}`.
func orDict(v any) any {
	if !truthy(v) {
		return NewDict()
	}
	return v
}

// items is v.items() (AttributeError when v is not a dict).
func items(v any) *Dict {
	d, ok := v.(*Dict)
	if !ok {
		raise("AttributeError", "'%s' object has no attribute 'items'", typeName(v))
	}
	return d
}

// dictOf is dict(v).
func dictOf(v any) *Dict {
	switch t := v.(type) {
	case *Dict:
		return t.Copy()
	case []any:
		out := NewDict()
		for i, x := range t {
			pair := iterPair(x, i)
			k, _ := hashKey(pair[0])
			out.Set(k, pair[1])
		}
		return out
	case string:
		if t == "" {
			return NewDict()
		}
		raise("ValueError", "dictionary update sequence element #0 has length 1; 2 is required")
	}
	raise("TypeError", "'%s' object is not iterable", typeName(v))
	return nil
}

func iterPair(x any, i int) []any {
	switch t := x.(type) {
	case []any, string, *Dict:
		items := iter(t)
		if len(items) != 2 {
			raise("ValueError", "dictionary update sequence element #%d has length %d; 2 is required", i, len(items))
		}
		return items
	}
	raise("TypeError", "cannot convert dictionary update sequence element #%d to a sequence", i)
	return nil
}

// listOf is list(v).
func listOf(v any) []any { return append([]any{}, iter(v)...) }

// pyInt is Python int(v).
func pyInt(v any) int64 {
	switch t := v.(type) {
	case bool:
		if t {
			return 1
		}
		return 0
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		if math.IsInf(t, 0) {
			raise("OverflowError", "cannot convert float infinity to integer")
		}
		if math.IsNaN(t) {
			raise("ValueError", "cannot convert float NaN to integer")
		}
		return int64(t)
	case string:
		s := pyfmt.Strip(t)
		body := s
		if strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
			body = body[1:]
		}
		if digitsOK(body) {
			if n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64); err == nil {
				return n
			}
		}
		raise("ValueError", "invalid literal for int() with base 10: %s", pyfmt.StrRepr(t))
	}
	raise("TypeError", "int() argument must be a string, a bytes-like object or a real number, not '%s'", typeName(v))
	return 0
}

// digitsOK reports whether s is digits with single underscores between them.
func digitsOK(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '_':
			if i == 0 || i == len(s)-1 || s[i-1] < '0' || s[i-1] > '9' || s[i+1] < '0' || s[i+1] > '9' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// pyFloat is Python float(v); ok is false where Python raises TypeError or ValueError.
func pyFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case bool, int64, int, float64:
		return num(t)
	case string:
		s := pyfmt.Strip(t)
		if strings.Contains(s, "_") {
			for i := 0; i < len(s); i++ {
				if s[i] == '_' && (i == 0 || i == len(s)-1 || !isDigit(s[i-1]) || !isDigit(s[i+1])) {
					return 0, false
				}
			}
			s = strings.ReplaceAll(s, "_", "")
		}
		if strings.ContainsAny(s, "xXpP") || s == "" {
			return 0, false
		}
		switch strings.ToLower(strings.TrimLeft(s, "+-")) {
		case "inf", "infinity", "nan":
			neg := strings.HasPrefix(s, "-")
			if strings.Count(s, "+")+strings.Count(s, "-") > 1 {
				return 0, false
			}
			if strings.EqualFold(strings.TrimLeft(s, "+-"), "nan") {
				return math.NaN(), true
			}
			if neg {
				return math.Inf(-1), true
			}
			return math.Inf(1), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return f, true
			}
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// sortedStrings sorts Python str values (code point order is UTF-8 byte order).
func sortedStrings(items []string) []string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return out
}
