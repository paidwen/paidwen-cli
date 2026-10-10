package pointer

import (
	"encoding/json"
	"testing"
)

func document(t *testing.T) any {
	t.Helper()
	var value any
	text := `{"foo":["bar","baz"],"":0,"a/b":1,"c%d":2,"e^f":3,"g|h":4,"i\\j":5,"k\"l":6," ":7,"m~n":8,"tool_input":{"command":"ls"}}`
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestThePointersOfRFC6901FindTheirValues(t *testing.T) {
	doc := document(t)
	cases := map[string]any{
		"/foo/0":              "bar",
		"/":                   float64(0),
		"/a~1b":               float64(1),
		"/c%d":                float64(2),
		"/m~0n":               float64(8),
		"/ ":                  float64(7),
		"/tool_input/command": "ls",
	}
	for text, want := range cases {
		parsed, err := Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		got, ok := parsed.Find(doc)
		if !ok || got != want {
			t.Fatalf("%s: got %v, %v", text, got, ok)
		}
	}
}

func TestTheEmptyPointerFindsTheWholeDocument(t *testing.T) {
	doc := document(t)
	parsed, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parsed.Find(doc)
	if !ok {
		t.Fatal("the empty pointer finds nothing")
	}
	if _, isObject := got.(map[string]any); !isObject {
		t.Fatalf("got %T", got)
	}
}

func TestAMissingValueIsNotFound(t *testing.T) {
	doc := document(t)
	for _, text := range []string{"/missing", "/foo/2", "/foo/01", "/foo/-", "/tool_input/command/x"} {
		parsed, err := Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got, ok := parsed.Find(doc); ok {
			t.Fatalf("%s found %v", text, got)
		}
	}
}

func TestAMalformedPointerIsRefused(t *testing.T) {
	for _, text := range []string{"tool_name", "/a~2", "/a~"} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("%s is accepted", text)
		}
	}
}
