package canonjson

import (
	"strings"
	"testing"
)

func TestVectors(t *testing.T) {
	type S struct {
		B int    `json:"b"`
		A string `json:"a"`
		C string `json:"c,omitempty"`
	}
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"sorted keys", map[string]any{"b": 1, "a": "x"}, `{"a":"x","b":1}`},
		{"struct omitempty", S{B: 2, A: "y"}, `{"a":"y","b":2}`},
		{"nested and arrays", map[string]any{"z": []any{map[string]any{"b": true, "a": false}, 3}}, `{"z":[{"a":false,"b":true},3]}`},
		{"escapes", "a\"b\\c\n\x01</>&/é ", "\"a\\\"b\\\\c\\n\\u0001</>&/é \""},
		{"negative int", map[string]any{"n": -12}, `{"n":-12}`},
		{"int64 max", map[string]any{"n": int64(9223372036854775807)}, `{"n":9223372036854775807}`},
		{"empty containers", map[string]any{"o": map[string]any{}, "a": []any{}}, `{"a":[],"o":{}}`},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q err=%v, want %q", c.name, got, err, c.want)
		}
	}
}

func TestRejects(t *testing.T) {
	for name, in := range map[string]string{
		"float":            `{"a":1.5}`,
		"exponent":         `{"a":1e3}`,
		"integer-as-float": `{"a":1.0}`,
		"null":             `{"a":null}`,
		"leading zero":     `{"a":01}`,
		"minus zero":       `{"a":-0}`,
		"overflow":         `{"a":9223372036854775808}`,
		"trailing":         `{"a":1} {"b":2}`,
	} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s: %s was accepted", name, in)
		}
	}
	if _, err := Marshal(map[string]any{"f": 1.5}); err == nil {
		t.Error("a float field was accepted")
	}
}

func TestWhitespaceAndOrderInsensitive(t *testing.T) {
	a, _ := Canonicalize([]byte(" {\n \"b\" : [1, 2],\n \"a\" : \"x\" } "))
	b, _ := Canonicalize([]byte(`{"a":"x","b":[1,2]}`))
	if string(a) != string(b) || string(a) != `{"a":"x","b":[1,2]}` {
		t.Errorf("got %q / %q", a, b)
	}
	if !strings.HasPrefix(string(a), "{") {
		t.Fatal("unexpected")
	}
}
