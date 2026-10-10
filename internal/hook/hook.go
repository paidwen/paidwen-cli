package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/paidwen/paidwen-cli/internal/pointer"
)

const maxInput = 8 << 20

type pointerFlag struct {
	set   bool
	value pointer.Pointer
}

func (f *pointerFlag) String() string {
	return f.value.String()
}

func (f *pointerFlag) Set(text string) error {
	parsed, err := pointer.Parse(text)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func (f *pointerFlag) text(document any) string {
	if !f.set {
		return ""
	}
	value, ok := f.value.Find(document)
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	}
	return ""
}

type Field struct {
	Name    string
	Pointer pointer.Pointer
}

type fieldsFlag []Field

func (f *fieldsFlag) String() string {
	names := make([]string, len(*f))
	for i, field := range *f {
		names[i] = field.Name + "=" + field.Pointer.String()
	}
	return strings.Join(names, ",")
}

func (f *fieldsFlag) Set(text string) error {
	name, path, ok := strings.Cut(text, "=")
	if !ok || name == "" {
		return errors.New("a -param reads name=pointer, for example cwd=/cwd")
	}
	parsed, err := pointer.Parse(path)
	if err != nil {
		return err
	}
	*f = append(*f, Field{Name: name, Pointer: parsed})
	return nil
}

type Spec struct {
	tool     pointerFlag
	toolName string
	params   pointerFlag
	fields   fieldsFlag
	session  pointerFlag
	call     pointerFlag
	server   pointerFlag
	kind     string
}

func (s *Spec) Register(set *flag.FlagSet) {
	set.Var(&s.tool, "tool", "the JSON pointer to the name of the tool in the input of the hook")
	set.StringVar(&s.toolName, "tool-name", "", "a fixed name of the tool, in place of -tool")
	set.Var(&s.params, "params", "the JSON pointer to the object of the parameters of the tool")
	set.Var(&s.fields, "param", "name=pointer: one more parameter read from the input of the hook, repeatable")
	set.Var(&s.session, "session", "the JSON pointer to the session")
	set.Var(&s.call, "call", "the JSON pointer to the identifier of the call, which links check and record")
	set.Var(&s.server, "server", "the JSON pointer to the server of the tool")
	set.StringVar(&s.kind, "kind", "function", "the kind of the tool: function, mcp or http")
}

func (s *Spec) Validate() error {
	if s.tool.set == (s.toolName != "") {
		return errors.New("give the tool with -tool or with -tool-name, and only one of them")
	}
	switch s.kind {
	case "function", "mcp", "http":
		return nil
	}
	return fmt.Errorf("the kind %q is not function, mcp or http", s.kind)
}

type Input struct {
	Tool    string
	Kind    string
	Server  string
	Params  map[string]any
	Session string
	Call    string
}

func Decode(reader io.Reader) (any, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, maxInput))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("the input of the hook is not JSON: %w", err)
	}
	return document, nil
}

func (s *Spec) Read(document any) (Input, error) {
	input := Input{Kind: s.kind, Params: map[string]any{}}
	if s.toolName != "" {
		input.Tool = s.toolName
	} else {
		input.Tool = s.tool.text(document)
		if input.Tool == "" {
			return Input{}, fmt.Errorf("the pointer %s finds no name of a tool in the input of the hook", s.tool.value)
		}
	}
	if s.params.set {
		value, ok := s.params.value.Find(document)
		object, isObject := value.(map[string]any)
		if !ok || !isObject {
			return Input{}, fmt.Errorf("the pointer %s finds no object of parameters in the input of the hook", s.params.value)
		}
		for name, parameter := range object {
			input.Params[name] = parameter
		}
	}
	for _, field := range s.fields {
		if value, ok := field.Pointer.Find(document); ok && value != nil {
			input.Params[field.Name] = value
		}
	}
	input.Session = s.session.text(document)
	input.Call = s.call.text(document)
	input.Server = s.server.text(document)
	return input, nil
}

func (i Input) Fingerprint(identity string) string {
	encoded, err := json.Marshal([]any{identity, i.Session, i.Tool, i.Kind, i.Server, i.Params})
	if err != nil {
		encoded = []byte(fmt.Sprint(identity, i.Session, i.Tool, i.Kind, i.Server, i.Params))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (i Input) CallKey(identity string) string {
	if i.Call == "" {
		return i.Fingerprint(identity)
	}
	sum := sha256.Sum256([]byte(identity + "\x00" + i.Session + "\x00" + i.Call))
	return hex.EncodeToString(sum[:])
}
