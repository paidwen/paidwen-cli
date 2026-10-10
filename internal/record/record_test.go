package record

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paidwen/paidwen-cli/internal/check"
	"github.com/paidwen/paidwen-cli/internal/contract"
	"github.com/paidwen/paidwen-cli/internal/credentials"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/hook"
	"github.com/paidwen/paidwen-cli/internal/state"
)

const (
	secret      = "pw_test_secret"
	preToolUse  = `{"session_id":"s-1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."},"tool_use_id":"toolu_1"}`
	postToolUse = `{"session_id":"s-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."},"tool_response":{"stdout":"ok","stderr":"","interrupted":false},"tool_use_id":"toolu_1"}`
)

var line = []string{"-tool", "/tool_name", "-params", "/tool_input", "-session", "/session_id", "-call", "/tool_use_id"}

type journal struct {
	t        *testing.T
	server   *httptest.Server
	env      env.Env
	start    time.Time
	mu       sync.Mutex
	now      time.Time
	parent   string
	actionID string
	bodies   [][]byte
	types    []string
	refuse   bool
}

func newJournal(t *testing.T) *journal {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Millisecond)
	j := &journal{t: t, start: start, now: start}
	j.server = httptest.NewServer(http.HandlerFunc(j.serve))
	t.Cleanup(j.server.Close)
	values := map[string]string{"PAIDWEN_API_KEY": secret, "PAIDWEN_BASE_URL": j.server.URL}
	j.env = env.Env{
		Version:   "0.1.0",
		Getenv:    func(name string) string { return values[name] },
		Now:       j.clock,
		ConfigDir: t.TempDir(),
		CacheDir:  t.TempDir(),
		HTTP:      &http.Client{},
	}
	return j
}

func (j *journal) clock() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.now
}

func (j *journal) advance(duration time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.now = j.now.Add(duration)
}

func (j *journal) refusing(refuse bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.refuse = refuse
}

func (j *journal) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+secret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/decisions":
		var request contract.DecisionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		j.mu.Lock()
		j.parent = r.Header.Get("traceparent")
		j.actionID = request.ActionID.String()
		now := j.now
		j.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.Decision{ActionID: request.ActionID, Verdict: contract.VerdictAllow, Observed: []contract.ObservedRule{}, DecidedAt: now})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/traces":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		j.mu.Lock()
		j.bodies = append(j.bodies, body)
		j.types = append(j.types, r.Header.Get("Content-Type"))
		refuse := j.refuse
		j.mu.Unlock()
		if refuse {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"https://paidwen.com/problems/unavailable","title":"The service cannot answer yet.","status":503,"detail":"The journal cannot take the spans yet. Send them again later."}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (j *journal) check(stdin string, args ...string) {
	j.t.Helper()
	var stdout, stderr bytes.Buffer
	if code := check.Run(context.Background(), j.env, args, strings.NewReader(stdin), &stdout, &stderr); code != 0 {
		j.t.Fatalf("paidwen check answered %d: %s", code, stderr.String())
	}
}

func (j *journal) record(stdin string, args ...string) (int, string) {
	j.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), j.env, args, strings.NewReader(stdin), &stdout, &stderr)
	if stdout.Len() != 0 {
		j.t.Fatalf("paidwen record printed %q on stdout", stdout.String())
	}
	return code, stderr.String()
}

func (j *journal) received() ([][]byte, []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.bodies), slices.Clone(j.types)
}

func (j *journal) decided() (string, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.parent, j.actionID
}

func (j *journal) kept(stdin string, args ...string) bool {
	j.t.Helper()
	set := flag.NewFlagSet("hook", flag.ContinueOnError)
	var spec hook.Spec
	spec.Register(set)
	if err := set.Parse(args); err != nil {
		j.t.Fatal(err)
	}
	document, err := hook.Decode(strings.NewReader(stdin))
	if err != nil {
		j.t.Fatal(err)
	}
	input, err := spec.Read(document)
	if err != nil {
		j.t.Fatal(err)
	}
	var call check.Call
	found, err := state.Store{Dir: j.env.CacheDir, Now: j.clock}.Get(state.Calls, input.CallKey(credentials.NewKey(secret, "").Identity()), &call)
	if err != nil {
		j.t.Fatal(err)
	}
	return found
}

func at(t *testing.T, node any, steps ...any) any {
	t.Helper()
	for _, step := range steps {
		switch key := step.(type) {
		case string:
			object, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("%v is not an object with %s", node, key)
			}
			node = object[key]
		case int:
			list, ok := node.([]any)
			if !ok || key >= len(list) {
				t.Fatalf("%v has no item %d", node, key)
			}
			node = list[key]
		}
	}
	return node
}

func texts(t *testing.T, list any) map[string]string {
	t.Helper()
	items, ok := list.([]any)
	if !ok {
		t.Fatalf("%v is not a list of attributes", list)
	}
	found := map[string]string{}
	for _, item := range items {
		key, _ := at(t, item, "key").(string)
		value, ok := at(t, item, "value").(map[string]any)
		content, isText := value["stringValue"].(string)
		if !ok || len(value) != 1 || !isText {
			t.Fatalf("the attribute %s is not a string value: %v", key, value)
		}
		found[key] = content
	}
	return found
}

func spanOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if service := texts(t, at(t, document, "resourceSpans", 0, "resource", "attributes")); !maps.Equal(service, map[string]string{"service.name": "paidwen-cli"}) {
		t.Fatalf("resource = %v", service)
	}
	if scope := at(t, document, "resourceSpans", 0, "scopeSpans", 0, "scope"); !maps.Equal(scope.(map[string]any), map[string]any{"name": "paidwen", "version": "0.1.0"}) {
		t.Fatalf("scope = %v", scope)
	}
	span, ok := at(t, document, "resourceSpans", 0, "scopeSpans", 0, "spans", 0).(map[string]any)
	if !ok {
		t.Fatal("no span")
	}
	return span
}

func nanosOf(at time.Time) string {
	return strconv.FormatInt(at.UnixNano(), 10)
}

func TestARecordSendsTheToolSpanWithItsResult(t *testing.T) {
	j := newJournal(t)
	j.check(preToolUse, line...)
	j.advance(2 * time.Second)
	code, stderr := j.record(postToolUse, append(slices.Clone(line), "-result", "/tool_response")...)
	if code != 0 || stderr != "" {
		t.Fatalf("got %d %q", code, stderr)
	}
	bodies, types := j.received()
	if len(bodies) != 1 || types[0] != "application/json" {
		t.Fatalf("%d bodies, %v", len(bodies), types)
	}
	parent, actionID := j.decided()
	ids := strings.Split(parent, "-")
	span := spanOf(t, bodies[0])
	if span["traceId"] != ids[1] || span["spanId"] != ids[2] || span["name"] != "execute_tool Bash" || span["kind"] != 1.0 {
		t.Fatalf("span = %v, traceparent %s", span, parent)
	}
	if span["startTimeUnixNano"] != nanosOf(j.start) || span["endTimeUnixNano"] != nanosOf(j.start.Add(2*time.Second)) {
		t.Fatalf("times = %v %v", span["startTimeUnixNano"], span["endTimeUnixNano"])
	}
	want := map[string]string{
		"gen_ai.operation.name":   "execute_tool",
		"gen_ai.tool.name":        "Bash",
		"paidwen.tool.kind":       "function",
		"paidwen.action.id":       actionID,
		"gen_ai.conversation.id":  "s-1",
		"gen_ai.tool.call.result": `{"interrupted":false,"stderr":"","stdout":"ok"}`,
	}
	if attributes := texts(t, span["attributes"]); !maps.Equal(attributes, want) {
		t.Fatalf("attributes = %v", attributes)
	}
	if name, when := at(t, span, "events", 0, "name"), at(t, span, "events", 0, "timeUnixNano"); name != "paidwen.action.started" || when != nanosOf(j.start) {
		t.Fatalf("events = %v", span["events"])
	}
	if code := at(t, span, "status", "code"); code != 1.0 {
		t.Fatalf("status = %v", span["status"])
	}
	if j.kept(postToolUse, line...) {
		t.Fatal("the recorded call stays")
	}
}

func TestARecordWithoutACallWarnsAndLetsTheAgentGoOn(t *testing.T) {
	j := newJournal(t)
	code, stderr := j.record(postToolUse, line...)
	if want := "Paidwen finds no call of Bash that paidwen check allowed on this machine, so the journal gets nothing.\n"; code != 0 || stderr != want {
		t.Fatalf("got %d %q", code, stderr)
	}
	if bodies, _ := j.received(); len(bodies) != 0 {
		t.Fatal("a call without check reaches the journal")
	}
}

func TestAFailedToolEndsInErrorAndAFailedSendKeepsTheCall(t *testing.T) {
	j := newJournal(t)
	stdin := `{"tool":"deploy","args":{"env":"prod"},"error":"exit status 1"}`
	hookLine := []string{"-tool", "/tool", "-params", "/args"}
	j.check(stdin, hookLine...)
	j.refusing(true)
	recordLine := append(slices.Clone(hookLine), "-error", "/error")
	code, stderr := j.record(stdin, recordLine...)
	if want := "Paidwen did not record deploy in the journal, and keeps the call for the next try: The journal cannot take the spans yet. Send them again later.\n"; code != 0 || stderr != want {
		t.Fatalf("got %d %q", code, stderr)
	}
	if !j.kept(stdin, hookLine...) {
		t.Fatal("a call that did not reach the journal is lost")
	}
	j.refusing(false)
	if code, stderr := j.record(stdin, recordLine...); code != 0 || stderr != "" {
		t.Fatalf("got %d %q", code, stderr)
	}
	bodies, _ := j.received()
	span := spanOf(t, bodies[1])
	if attributes := texts(t, span["attributes"]); attributes["error.type"] != "ToolError" || attributes["paidwen.tool.params"] != "" {
		t.Fatalf("attributes = %v", attributes)
	}
	if code := at(t, span, "status", "code"); code != 2.0 {
		t.Fatalf("status = %v", span["status"])
	}
	if j.kept(stdin, hookLine...) {
		t.Fatal("the recorded call stays")
	}
}

func TestAResultAbove8MiBIsLeftOut(t *testing.T) {
	j := newJournal(t)
	quotes := strings.Repeat(`\"`, 5<<19)
	post := `{"session_id":"s-1","tool_name":"Bash","tool_input":{"command":"cat big.json"},"tool_response":{"stdout":"` + quotes + `"},"tool_use_id":"toolu_1"}`
	j.check(post, line...)
	code, stderr := j.record(post, append(slices.Clone(line), "-result", "/tool_response")...)
	if want := "Paidwen leaves the result of Bash out of the journal, because the span would exceed 8 MiB.\n"; code != 0 || stderr != want {
		t.Fatalf("got %d %q", code, stderr)
	}
	bodies, _ := j.received()
	if len(bodies) != 1 || len(bodies[0]) > 8<<20 {
		t.Fatalf("%d bodies", len(bodies))
	}
	attributes := texts(t, spanOf(t, bodies[0])["attributes"])
	if _, kept := attributes["gen_ai.tool.call.result"]; kept || attributes["paidwen.tool.params"] != "" {
		t.Fatalf("attributes = %v", attributes)
	}
}
