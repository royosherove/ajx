package util

import "testing"

func TestPyFloatMatchesPythonRepr(t *testing.T) {
	cases := map[float64]string{
		1730000000.12: "1730000000.12", 0.1: "0.1", 1e-07: "1e-07", 0.0001: "0.0001",
		1e16: "1e+16", 1.5e16: "1.5e+16", 123456789012345.6: "123456789012345.6", -2.5: "-2.5",
		3: "3.0", 1e22: "1e+22", 0.00012: "0.00012", 1.234e-05: "1.234e-05",
	}
	for in, want := range cases {
		if got := PyFloat(in); got != want {
			t.Errorf("PyFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPyJSONSeparators(t *testing.T) {
	got := PyJSON(M{"b": []any{int64(1), "x"}, "a": nil})
	if want := `{"a": null, "b": [1, "x"]}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestJSONRoundTripKeepsPythonNumberSpelling(t *testing.T) {
	in := []byte(`{"int": 34, "float": 34.0, "small": 1e-07, "big": 12345678901234567890, "list": [1, 2.5], "s": "<a&b>"}`)
	v, err := DecodeJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(M)
	if _, ok := m["int"].(int64); !ok {
		t.Fatalf("int decoded as %T", m["int"])
	}
	if _, ok := m["float"].(float64); !ok {
		t.Fatalf("float decoded as %T", m["float"])
	}
	out, err := MarshalIndent(v)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"big\": 1.2345678901234567e+19,\n  \"float\": 34.0,\n  \"int\": 34,\n  \"list\": [\n    1,\n    2.5\n  ],\n  \"s\": \"<a&b>\",\n  \"small\": 1e-07\n}\n"
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if _, err := DecodeJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("extra data accepted")
	}
	type rec struct {
		A int     `json:"a"`
		B float64 `json:"b"`
	}
	out, _ = MarshalIndent(M{"r": rec{1, 2}, "e": M{}, "l": []string{}})
	if want := "{\n  \"e\": {},\n  \"l\": [],\n  \"r\": {\n    \"a\": 1,\n    \"b\": 2\n  }\n}\n"; string(out) != want {
		t.Fatalf("got %q", out)
	}
}
