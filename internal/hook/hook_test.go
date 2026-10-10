package hook

import (
	"flag"
	"strings"
	"testing"
)

const claudeCode = `{"session_id":"s-1","cwd":"/work","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git push origin main","timeout":120000},"tool_use_id":"toolu_1"}`

func spec(t *testing.T, args ...string) *Spec {
	t.Helper()
	set := flag.NewFlagSet("check", flag.ContinueOnError)
	var s Spec
	s.Register(set)
	if err := set.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return &s
}

func read(t *testing.T, s *Spec, text string) Input {
	t.Helper()
	document, err := Decode(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.Read(document)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestTheLineOfClaudeCodeReadsTheToolTheParametersAndTheSession(t *testing.T) {
	s := spec(t, "-tool", "/tool_name", "-params", "/tool_input", "-param", "cwd=/cwd", "-session", "/session_id", "-call", "/tool_use_id")
	input := read(t, s, claudeCode)
	if input.Tool != "Bash" || input.Kind != "function" || input.Session != "s-1" || input.Call != "toolu_1" {
		t.Fatalf("got %+v", input)
	}
	if input.Params["command"] != "git push origin main" || input.Params["cwd"] != "/work" {
		t.Fatalf("got %+v", input.Params)
	}
	if input.Params["timeout"].(interface{ String() string }).String() != "120000" {
		t.Fatalf("the number changed: %v", input.Params["timeout"])
	}
}

func TestAFixedToolNameAndNamedParametersReadAShellHook(t *testing.T) {
	s := spec(t, "-tool-name", "shell", "-param", "command=/command", "-param", "cwd=/cwd", "-param", "absent=/absent", "-session", "/conversation_id")
	input := read(t, s, `{"command":"rm -rf build","cwd":"/work","conversation_id":"c-9"}`)
	if input.Tool != "shell" || input.Session != "c-9" || len(input.Params) != 2 {
		t.Fatalf("got %+v", input)
	}
}

func TestATooShortInputIsRefused(t *testing.T) {
	s := spec(t, "-tool", "/tool_name", "-params", "/tool_input")
	document, err := Decode(strings.NewReader(`{"tool_input":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(document); err == nil {
		t.Fatal("an input without a tool name is accepted")
	}
	document, err = Decode(strings.NewReader(`{"tool_name":"Bash","tool_input":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(document); err == nil {
		t.Fatal("parameters that are not an object are accepted")
	}
	if _, err := Decode(strings.NewReader(`not json`)); err == nil {
		t.Fatal("an input that is not JSON is accepted")
	}
}

func TestTheToolComesFromOnePlaceOnly(t *testing.T) {
	for _, args := range [][]string{{}, {"-tool", "/tool_name", "-tool-name", "Bash"}, {"-tool-name", "Bash", "-kind", "shell"}} {
		set := flag.NewFlagSet("check", flag.ContinueOnError)
		var s Spec
		s.Register(set)
		if err := set.Parse(args); err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(); err == nil {
			t.Fatalf("%v is accepted", args)
		}
	}
}

func TestTheSameRequestGivesTheSameFingerprint(t *testing.T) {
	s := spec(t, "-tool", "/tool_name", "-params", "/tool_input", "-session", "/session_id", "-call", "/tool_use_id")
	first := read(t, s, claudeCode)
	second := read(t, s, strings.Replace(claudeCode, "toolu_1", "toolu_2", 1))
	if first.Fingerprint("key") != second.Fingerprint("key") {
		t.Fatal("two retries of the same command differ")
	}
	if first.CallKey("key") == second.CallKey("key") {
		t.Fatal("two calls share a key")
	}
	if first.Fingerprint("key") == first.Fingerprint("other") {
		t.Fatal("two agents share a fingerprint")
	}
}
