package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/paidwen/paidwen-cli/internal/api"
	"github.com/paidwen/paidwen-cli/internal/check"
	"github.com/paidwen/paidwen-cli/internal/credentials"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/hook"
	"github.com/paidwen/paidwen-cli/internal/pointer"
	"github.com/paidwen/paidwen-cli/internal/state"
)

const (
	maxBody          = 8 << 20
	serviceName      = "paidwen-cli"
	scopeName        = "paidwen"
	spanKindInternal = 1
	statusOK         = 1
	statusError      = 2
	actionStarted    = "paidwen.action.started"
	resultKey        = "gen_ai.tool.call.result"
)

type options struct {
	spec    hook.Spec
	result  *pointer.Pointer
	failure *pointer.Pointer
	timeout time.Duration
}

type exportRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type resource struct {
	Attributes []attribute `json:"attributes"`
}

type scopeSpans struct {
	Scope scope  `json:"scope"`
	Spans []span `json:"spans"`
}

type scope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type span struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []attribute `json:"attributes"`
	Events            []event     `json:"events"`
	Status            status      `json:"status"`
}

type attribute struct {
	Key   string `json:"key"`
	Value value  `json:"value"`
}

type value struct {
	StringValue string `json:"stringValue"`
}

type event struct {
	TimeUnixNano string `json:"timeUnixNano"`
	Name         string `json:"name"`
}

type status struct {
	Code int `json:"code"`
}

type exportResponse struct {
	PartialSuccess struct {
		RejectedSpans json.Number `json:"rejectedSpans"`
		ErrorMessage  string      `json:"errorMessage"`
	} `json:"partialSuccess"`
}

func Run(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o options
	set := flag.NewFlagSet("paidwen record", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	o.register(set)
	err := set.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		set.SetOutput(stderr)
		say(stderr, "Usage: paidwen record [flags], with the input of the hook on stdin")
		set.PrintDefaults()
		return 0
	}
	if err == nil {
		err = o.validate(set.Args())
	}
	if err == nil {
		err = o.record(ctx, e, stdin, stderr)
	}
	if err != nil {
		say(stderr, "Paidwen did not record the call in the journal: "+sentence(err.Error()))
	}
	return 0
}

func (o *options) register(set *flag.FlagSet) {
	o.spec.Register(set)
	set.Func("result", "the JSON pointer to the result of the tool", into(&o.result))
	set.Func("error", "the JSON pointer to the error of the tool: true or a text that is not empty means the tool failed", into(&o.failure))
	set.DurationVar(&o.timeout, "timeout", 3*time.Second, "how long to wait for the answer of Paidwen")
}

func into(target **pointer.Pointer) func(string) error {
	return func(text string) error {
		parsed, err := pointer.Parse(text)
		if err != nil {
			return err
		}
		*target = &parsed
		return nil
	}
}

func (o *options) validate(rest []string) error {
	if len(rest) > 0 {
		return fmt.Errorf("paidwen record takes no argument, and got %q", strings.Join(rest, " "))
	}
	if err := o.spec.Validate(); err != nil {
		return err
	}
	if o.timeout <= 0 {
		return errors.New("-timeout is not positive")
	}
	return nil
}

func (o *options) record(ctx context.Context, e env.Env, stdin io.Reader, stderr io.Writer) error {
	document, err := hook.Decode(stdin)
	if err != nil {
		return err
	}
	input, err := o.spec.Read(document)
	if err != nil {
		return err
	}
	store := state.Store{Dir: e.CacheDir, Now: e.Now}
	resolved, err := credentials.Resolve(e, store)
	if err != nil {
		return err
	}
	key := input.CallKey(resolved.Provider.Identity())
	var call check.Call
	if found, _ := store.Get(state.Calls, key, &call); !found {
		say(stderr, "Paidwen finds no call of "+input.Tool+" that paidwen check allowed on this machine, so the journal gets nothing.")
		return nil
	}
	result, hasResult, err := o.resultOf(document)
	if err != nil {
		return err
	}
	failed := o.failedOf(document)
	ended := e.Now()
	build := func(content ...attribute) ([]byte, error) {
		return marshal(exportOf(e.Version, input, call, content, failed, ended))
	}
	var content []attribute
	if hasResult {
		content = append(content, text(resultKey, result))
	}
	body, err := build(content...)
	if err == nil && hasResult && len(body) > maxBody {
		say(stderr, "Paidwen leaves the result of "+input.Tool+" out of the journal, because the span would exceed 8 MiB.")
		body, err = build()
	}
	if err != nil {
		return err
	}
	client := api.Client{BaseURL: resolved.BaseURL, Credentials: resolved.Provider, HTTP: e.HTTP, UserAgent: e.UserAgent()}
	sent, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	var answer json.RawMessage
	if _, err := client.Send(sent, http.MethodPost, "/v1/traces", nil, body, &answer); err != nil {
		say(stderr, "Paidwen did not record "+input.Tool+" in the journal, and keeps the call for the next try: "+sentence(err.Error()))
		return nil
	}
	if message, rejected := rejection(answer); rejected {
		say(stderr, "Paidwen rejected the span of "+input.Tool+": "+sentence(message))
	}
	_ = store.Delete(state.Calls, key)
	return nil
}

func (o *options) resultOf(document any) (string, bool, error) {
	if o.result == nil {
		return "", false, nil
	}
	found, ok := o.result.Find(document)
	if !ok || found == nil {
		return "", false, nil
	}
	if typed, isText := found.(string); isText {
		return typed, true, nil
	}
	encoded, err := marshal(found)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

func (o *options) failedOf(document any) bool {
	if o.failure == nil {
		return false
	}
	found, ok := o.failure.Find(document)
	if !ok {
		return false
	}
	switch typed := found.(type) {
	case bool:
		return typed
	case string:
		return typed != ""
	}
	return false
}

func exportOf(version string, input hook.Input, call check.Call, extra []attribute, failed bool, ended time.Time) exportRequest {
	attributes := []attribute{
		text("gen_ai.operation.name", "execute_tool"),
		text("gen_ai.tool.name", input.Tool),
		text("paidwen.tool.kind", input.Kind),
	}
	if input.Server != "" {
		attributes = append(attributes, text("paidwen.tool.server", input.Server))
	}
	attributes = append(attributes, text("paidwen.action.id", call.ActionID))
	if input.Session != "" {
		attributes = append(attributes, text("gen_ai.conversation.id", input.Session))
	}
	attributes = append(attributes, extra...)
	code := statusOK
	if failed {
		attributes = append(attributes, text("error.type", "ToolError"))
		code = statusError
	}
	return exportRequest{ResourceSpans: []resourceSpans{{
		Resource: resource{Attributes: []attribute{text("service.name", serviceName)}},
		ScopeSpans: []scopeSpans{{
			Scope: scope{Name: scopeName, Version: version},
			Spans: []span{{
				TraceID:           call.TraceID,
				SpanID:            call.SpanID,
				Name:              "execute_tool " + input.Tool,
				Kind:              spanKindInternal,
				StartTimeUnixNano: nanos(call.StartedAt),
				EndTimeUnixNano:   nanos(ended),
				Attributes:        attributes,
				Events:            []event{{TimeUnixNano: nanos(call.DecidedAt), Name: actionStarted}},
				Status:            status{Code: code},
			}},
		}},
	}}}
}

func rejection(answer json.RawMessage) (string, bool) {
	var response exportResponse
	if json.Unmarshal(answer, &response) != nil {
		return "", false
	}
	rejected, err := response.PartialSuccess.RejectedSpans.Int64()
	if err != nil || rejected == 0 {
		return "", false
	}
	if response.PartialSuccess.ErrorMessage == "" {
		return "the journal keeps none of it", true
	}
	return response.PartialSuccess.ErrorMessage, true
}

func text(key, content string) attribute {
	return attribute{Key: key, Value: value{StringValue: content}}
}

func nanos(at time.Time) string {
	return strconv.FormatInt(at.UnixNano(), 10)
}

func marshal(content any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(content); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func sentence(content string) string {
	content = strings.TrimSpace(content)
	if content == "" || strings.HasSuffix(content, ".") {
		return content
	}
	return content + "."
}

func say(w io.Writer, line string) {
	_, _ = io.WriteString(w, line+"\n")
}
