package check

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/paidwen/paidwen-cli/internal/contract"
	"github.com/paidwen/paidwen-cli/internal/credentials"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/hook"
	"github.com/paidwen/paidwen-cli/internal/state"
)

const (
	secret       = "pw_test_secret"
	preToolUse   = `{"session_id":"s-1","cwd":"/work","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"rm -rf build"},"tool_use_id":"toolu_1","cost":0.25}`
	cursorOutput = `{"permission":"{{if .Allowed}}allow{{else}}deny{{end}}","agent_message":{{json .Reason}}}`
)

var (
	claudeLine = []string{"-tool", "/tool_name", "-params", "/tool_input", "-param", "cwd=/cwd", "-session", "/session_id", "-call", "/tool_use_id"}
	cursorLine = []string{"-tool-name", "shell", "-param", "command=/command", "-param", "cwd=/cwd", "-session", "/conversation_id", "-output", cursorOutput}
	approvalID = contract.ApprovalID(uuid.MustParse("0d9e8f7a-6b5c-4d3e-8f2a-1b0c9d8e7f6a"))
	incidentID = contract.IncidentID(uuid.MustParse("5b1f0c2e-7d4a-4e9b-8c3f-6a2d1e0b9c87"))
	identity   = credentials.NewKey(secret, "PAIDWEN_API_KEY").Identity()
)

type harness struct {
	t       *testing.T
	server  *httptest.Server
	env     env.Env
	values  map[string]string
	start   time.Time
	mu      sync.Mutex
	now     time.Time
	slept   []time.Duration
	asked   []map[string]any
	parents []string
	polls   int
	decide  func(w http.ResponseWriter, request contract.DecisionRequest)
	approve func(w http.ResponseWriter, attempt int)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Millisecond)
	h := &harness{t: t, start: start, now: start}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	h.values = map[string]string{"PAIDWEN_API_KEY": secret, "PAIDWEN_BASE_URL": h.server.URL}
	h.env = env.Env{
		Version:   "0.1.0",
		Getenv:    func(name string) string { return h.values[name] },
		Now:       h.clock,
		ConfigDir: t.TempDir(),
		CacheDir:  t.TempDir(),
		HTTP:      &http.Client{},
	}
	previous := sleep
	sleep = h.sleep
	t.Cleanup(func() { sleep = previous })
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) sleep(_ context.Context, duration time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(duration)
	h.slept = append(h.slept, duration)
	return nil
}

func (h *harness) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+secret {
		problem(w, http.StatusUnauthorized, "unauthorized", "The key is missing, malformed or unknown, or it opens another door.", "")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/decisions":
		body, err := io.ReadAll(r.Body)
		var request contract.DecisionRequest
		var raw map[string]any
		if err != nil || json.Unmarshal(body, &request) != nil || json.Unmarshal(body, &raw) != nil {
			problem(w, http.StatusBadRequest, "invalid-request", "The request does not follow the contract.", "")
			return
		}
		h.mu.Lock()
		h.asked = append(h.asked, raw)
		h.parents = append(h.parents, r.Header.Get("traceparent"))
		h.mu.Unlock()
		h.decide(w, request)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/approvals/"+approvalID.String():
		h.mu.Lock()
		h.polls++
		attempt := h.polls
		h.mu.Unlock()
		h.approve(w, attempt)
	default:
		problem(w, http.StatusNotFound, "not-found", "The resource does not exist.", "")
	}
}

func (h *harness) run(stdin string, args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), h.env, args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (h *harness) requests() ([]map[string]any, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked), slices.Clone(h.parents)
}

func (h *harness) pollCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.polls
}

func (h *harness) pauses() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.slept)
}

func (h *harness) decision(request contract.DecisionRequest, verdict contract.Verdict) contract.Decision {
	return contract.Decision{ActionID: request.ActionID, Verdict: verdict, Observed: []contract.ObservedRule{}, DecidedAt: h.clock()}
}

func (h *harness) waiting(request contract.DecisionRequest, onExpiry contract.OnExpiry) contract.Decision {
	decision := ruled(h.decision(request, contract.VerdictRequireApproval), "rule-delete", "Deleting needs a human")
	decision.Approval = &contract.Approval{ID: approvalID, ExpiresIn: 300, Interval: 5, OnExpiry: &onExpiry}
	return decision
}

func (h *harness) status(w http.ResponseWriter, answer contract.ApprovalState, onExpiry contract.OnExpiry) {
	reply(w, contract.ApprovalStatus{
		ID:        approvalID,
		ActionID:  contract.ActionID(uuid.MustParse("6f1c2a7e-3b5d-4c8e-9a10-2f4b6d8e0a12")),
		Status:    answer,
		ExpiresAt: h.start.Add(300 * time.Second),
		Interval:  5,
		OnExpiry:  onExpiry,
	})
}

func (h *harness) stored(kind, key string, value any) bool {
	h.t.Helper()
	found, err := state.Store{Dir: h.env.CacheDir, Now: h.clock}.Get(kind, key, value)
	if err != nil {
		h.t.Fatal(err)
	}
	return found
}

func ruled(decision contract.Decision, id, name string) contract.Decision {
	decision.RuleID = &id
	decision.RuleName = &name
	return decision
}

func reply(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func problem(w http.ResponseWriter, code int, slug, title, detail string) {
	body := contract.Problem{Type: "https://paidwen.com/problems/" + slug, Title: title, Status: int32(code)}
	if detail != "" {
		body.Detail = &detail
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func claude(extra ...string) []string {
	return append(slices.Clone(claudeLine), extra...)
}

func inputOf(t *testing.T, stdin string, args ...string) hook.Input {
	t.Helper()
	set := flag.NewFlagSet("hook", flag.ContinueOnError)
	var spec hook.Spec
	spec.Register(set)
	if err := set.Parse(args); err != nil {
		t.Fatal(err)
	}
	document, err := hook.Decode(strings.NewReader(stdin))
	if err != nil {
		t.Fatal(err)
	}
	input, err := spec.Read(document)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func objectAt(t *testing.T, document map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := document[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in %v", key, document)
	}
	return value
}

func TestAnAllowedActionPassesSilentlyAndKeepsItsCall(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		reply(w, h.decision(request, contract.VerdictAllow))
	}
	code, stdout, stderr := h.run(preToolUse, claude("-cost", "/cost")...)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
	asked, parents := h.requests()
	if len(asked) != 1 {
		t.Fatalf("%d decisions asked", len(asked))
	}
	body := asked[0]
	actionID, _ := body["action_id"].(string)
	if id, err := uuid.Parse(actionID); err != nil || id[6]>>4 != 4 {
		t.Fatalf("the action id %q is not a random UUID", actionID)
	}
	tool := objectAt(t, body, "tool")
	if _, server := tool["server"]; tool["name"] != "Bash" || tool["kind"] != "function" || server {
		t.Fatalf("tool = %v", tool)
	}
	params := objectAt(t, body, "params")
	if len(params) != 2 || params["command"] != "rm -rf build" || params["cwd"] != "/work" {
		t.Fatalf("params = %v", params)
	}
	if cost := objectAt(t, body, "cost"); cost["estimated_usd"] != 0.25 {
		t.Fatalf("cost = %v", cost)
	}
	known := objectAt(t, body, "context")
	traceID, _ := known["trace_id"].(string)
	if known["session_id"] != "s-1" || len(traceID) != 32 {
		t.Fatalf("context = %v", known)
	}
	parts := strings.Split(parents[0], "-")
	if len(parts) != 4 || parts[0] != "00" || parts[1] != traceID || len(parts[2]) != 16 || parts[3] != "01" {
		t.Fatalf("traceparent = %q", parents[0])
	}
	var call Call
	if !h.stored(state.Calls, inputOf(t, preToolUse, claudeLine...).CallKey(identity), &call) {
		t.Fatal("the allowed call is not kept for paidwen record")
	}
	if call.ActionID != actionID || call.TraceID != traceID || call.SpanID != parts[2] || !call.StartedAt.Equal(h.start) || !call.DecidedAt.Equal(h.start) {
		t.Fatalf("call = %+v", call)
	}
}

func TestARefusalSaysWhyAndKeepsNoCall(t *testing.T) {
	cases := []struct {
		name   string
		answer func(contract.Decision) contract.Decision
		want   string
	}{
		{
			name: "a rule",
			answer: func(decision contract.Decision) contract.Decision {
				return ruled(decision, "rule-push", "No force push")
			},
			want: `Paidwen blocked Bash: the rule "No force push" refuses it.`,
		},
		{
			name: "a budget",
			answer: func(decision contract.Decision) contract.Decision {
				decision.Budget = &contract.BudgetOutcome{Exceeded: contract.BudgetWindowDaily, SpentUsd: 9.8, LimitUsd: 10}
				return decision
			},
			want: "Paidwen blocked Bash: the daily budget of the agent is $10.00, and the agent already spent $9.80.",
		},
		{
			name: "the permissions",
			answer: func(decision contract.Decision) contract.Decision {
				decision.Permission = &contract.PermissionOutcome{Allowed: false}
				return decision
			},
			want: "Paidwen blocked Bash: the permissions of the agent do not allow it.",
		},
		{
			name: "a quarantine",
			answer: func(decision contract.Decision) contract.Decision {
				decision.Verdict = contract.VerdictQuarantine
				decision.Incident = &contract.IncidentReference{ID: incidentID, Kind: contract.IncidentKindQuarantine}
				return decision
			},
			want: "Paidwen blocked Bash: the agent is in quarantine (incident " + incidentID.String() + ").",
		},
		{
			name: "a rule that quarantines",
			answer: func(decision contract.Decision) contract.Decision {
				decision.Verdict = contract.VerdictQuarantine
				decision.Incident = &contract.IncidentReference{ID: incidentID, Kind: contract.IncidentKindQuarantine}
				return ruled(decision, "rule-secrets", "No secrets out")
			},
			want: `Paidwen blocked Bash: the rule "No secrets out" refuses it, and the agent is in quarantine (incident ` + incidentID.String() + ").",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
				reply(w, c.answer(h.decision(request, contract.VerdictBlock)))
			}
			code, stdout, stderr := h.run(preToolUse, claudeLine...)
			if code != 2 || stdout != "" || stderr != c.want+"\n" {
				t.Fatalf("got %d %q %q", code, stdout, stderr)
			}
			var call Call
			if h.stored(state.Calls, inputOf(t, preToolUse, claudeLine...).CallKey(identity), &call) {
				t.Fatal("a refused action keeps a call")
			}
		})
	}
}

func TestAnApprovalLetsTheActionRunAfterTwoPolls(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		reply(w, h.waiting(request, contract.OnExpiryDeny))
	}
	h.approve = func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			h.status(w, contract.ApprovalStatePending, contract.OnExpiryDeny)
			return
		}
		h.status(w, contract.ApprovalStateApproved, contract.OnExpiryDeny)
	}
	code, stdout, stderr := h.run(preToolUse, claudeLine...)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
	if polls, slept := h.pollCount(), h.pauses(); polls != 2 || !slices.Equal(slept, []time.Duration{5 * time.Second}) {
		t.Fatalf("%d polls, pauses %v", polls, slept)
	}
	input := inputOf(t, preToolUse, claudeLine...)
	var call Call
	if !h.stored(state.Calls, input.CallKey(identity), &call) || !call.StartedAt.Equal(h.start) || !call.DecidedAt.Equal(h.start.Add(5*time.Second)) {
		t.Fatalf("call = %+v", call)
	}
	var held pending
	if h.stored(state.Pending, input.Fingerprint(identity), &held) {
		t.Fatal("an approved action stays pending")
	}
}

func TestAnApprovalLongerThanTheWaitResumesWithTheSameRequest(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		reply(w, h.waiting(request, contract.OnExpiryDeny))
	}
	h.approve = func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			h.status(w, contract.ApprovalStatePending, contract.OnExpiryDeny)
			return
		}
		h.status(w, contract.ApprovalStateApproved, contract.OnExpiryDeny)
	}
	code, stdout, stderr := h.run(preToolUse, claude("-wait", "3s")...)
	want := `Paidwen holds Bash: the rule "Deleting needs a human" asks for an approval. The approval ` + approvalID.String() +
		" waits in Paidwen until " + h.start.Add(300*time.Second).Format(time.RFC3339) +
		". Call Bash again later with the same parameters, and it runs once someone approves it.\n"
	if code != 2 || stdout != "" || stderr != want {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
	if slept := h.pauses(); len(slept) != 0 {
		t.Fatalf("the command waited %v beyond -wait", slept)
	}
	first := inputOf(t, preToolUse, claudeLine...)
	var held pending
	if !h.stored(state.Pending, first.Fingerprint(identity), &held) || held.ApprovalID != approvalID.String() {
		t.Fatalf("pending = %+v", held)
	}
	_ = h.sleep(context.Background(), 2*time.Minute)
	again := strings.Replace(preToolUse, "toolu_1", "toolu_2", 1)
	code, stdout, stderr = h.run(again, claudeLine...)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
	asked, _ := h.requests()
	if len(asked) != 1 || h.pollCount() != 2 {
		t.Fatalf("%d decisions and %d polls", len(asked), h.pollCount())
	}
	var call Call
	if !h.stored(state.Calls, inputOf(t, again, claudeLine...).CallKey(identity), &call) {
		t.Fatal("the resumed call is not kept")
	}
	if call.ActionID != asked[0]["action_id"] || call.TraceID != held.TraceID || !call.StartedAt.Equal(h.start) || !call.DecidedAt.Equal(h.start.Add(2*time.Minute)) {
		t.Fatalf("call = %+v", call)
	}
	if h.stored(state.Pending, first.Fingerprint(identity), &held) {
		t.Fatal("an approved action stays pending")
	}
}

func TestADeniedApprovalRefuses(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		reply(w, h.waiting(request, contract.OnExpiryAllow))
	}
	h.approve = func(w http.ResponseWriter, _ int) {
		h.status(w, contract.ApprovalStateDenied, contract.OnExpiryAllow)
	}
	code, _, stderr := h.run(preToolUse, claudeLine...)
	if want := "Paidwen blocked Bash: the rule \"Deleting needs a human\" asks for an approval, and the approver denied it.\n"; code != 2 || stderr != want {
		t.Fatalf("got %d %q", code, stderr)
	}
}

func TestAnExpiredApprovalFollowsOnExpiry(t *testing.T) {
	for _, c := range []struct {
		onExpiry contract.OnExpiry
		code     int
		stderr   string
	}{
		{contract.OnExpiryAllow, 0, ""},
		{contract.OnExpiryDeny, 2, "Paidwen blocked Bash: the rule \"Deleting needs a human\" asks for an approval, and the wait expired without an answer.\n"},
	} {
		t.Run(string(c.onExpiry), func(t *testing.T) {
			h := newHarness(t)
			h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
				reply(w, h.waiting(request, c.onExpiry))
			}
			h.approve = func(w http.ResponseWriter, _ int) {
				h.status(w, contract.ApprovalStateExpired, c.onExpiry)
			}
			code, _, stderr := h.run(preToolUse, claudeLine...)
			if code != c.code || stderr != c.stderr {
				t.Fatalf("got %d %q", code, stderr)
			}
			var call Call
			if kept := h.stored(state.Calls, inputOf(t, preToolUse, claudeLine...).CallKey(identity), &call); kept != (c.code == 0) {
				t.Fatalf("the call kept is %v", kept)
			}
		})
	}
}

func TestASlowDownWidensTheIntervalAndWaitsRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		reply(w, h.waiting(request, contract.OnExpiryDeny))
	}
	h.approve = func(w http.ResponseWriter, attempt int) {
		switch attempt {
		case 1:
			w.Header().Set("Retry-After", "12")
			problem(w, http.StatusTooManyRequests, "slow-down", "The agent polls faster than the interval.", "Poll at most once per interval, and add 5 seconds to the interval for every later poll.")
		case 2:
			h.status(w, contract.ApprovalStatePending, contract.OnExpiryDeny)
		default:
			h.status(w, contract.ApprovalStateApproved, contract.OnExpiryDeny)
		}
	}
	code, _, stderr := h.run(preToolUse, claudeLine...)
	if code != 0 || stderr != "" {
		t.Fatalf("got %d %q", code, stderr)
	}
	if slept := h.pauses(); !slices.Equal(slept, []time.Duration{12 * time.Second, 10 * time.Second}) {
		t.Fatalf("pauses %v", slept)
	}
}

func TestAnErrorOfTheRequestRefusesWithItsReason(t *testing.T) {
	disabled := "The agent laptop-toky is disabled. Paidwen refuses every request of the agent until someone enables it."
	cases := []struct {
		name   string
		key    string
		answer func(w http.ResponseWriter)
		want   string
	}{
		{
			name: "a refused key",
			key:  "pw_revoked",
			want: "Paidwen refused the key from PAIDWEN_API_KEY. Run paidwen login to sign in again.",
		},
		{
			name: "a disabled agent",
			key:  secret,
			answer: func(w http.ResponseWriter) {
				problem(w, http.StatusForbidden, "agent-disabled", "The agent is disabled.", disabled)
			},
			want: "Paidwen refused the agent: " + disabled,
		},
		{
			name: "a request too large",
			key:  secret,
			answer: func(w http.ResponseWriter) {
				problem(w, http.StatusRequestEntityTooLarge, "payload-too-large", "The request body is larger than the service accepts.", "The body must not exceed 1 MiB.")
			},
			want: "Paidwen refused the request: The body must not exceed 1 MiB.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.values["PAIDWEN_API_KEY"] = c.key
			h.decide = func(w http.ResponseWriter, _ contract.DecisionRequest) {
				c.answer(w)
			}
			code, stdout, stderr := h.run(preToolUse, claude("-on-unavailable", "allow")...)
			if code != 2 || stdout != "" || stderr != c.want+"\n" {
				t.Fatalf("got %d %q %q", code, stdout, stderr)
			}
		})
	}
}

func TestAnUnavailablePaidwenFollowsOnUnavailable(t *testing.T) {
	quota := "The organization reached the 10000 events its plan allows in October 2026."
	for _, c := range []struct {
		choice string
		code   int
		stderr string
	}{
		{"block", 2, "Paidwen answered HTTP 503, so paidwen check blocks Bash as -on-unavailable block asks. " + quota + "\n"},
		{"allow", 0, "Paidwen answered HTTP 503, so paidwen check allows Bash as -on-unavailable allow asks. " + quota + "\n"},
	} {
		t.Run(c.choice, func(t *testing.T) {
			h := newHarness(t)
			h.decide = func(w http.ResponseWriter, _ contract.DecisionRequest) {
				problem(w, http.StatusServiceUnavailable, "quota-exceeded", "The organization reached the events its plan allows this month.", quota)
			}
			code, stdout, stderr := h.run(preToolUse, claude("-on-unavailable", c.choice)...)
			if code != c.code || stdout != "" || stderr != c.stderr {
				t.Fatalf("got %d %q %q", code, stdout, stderr)
			}
			var call Call
			if kept := h.stored(state.Calls, inputOf(t, preToolUse, claudeLine...).CallKey(identity), &call); kept != (c.code == 0) {
				t.Fatalf("the call kept is %v", kept)
			}
		})
	}
}

func TestAPaidwenThatIsDownBlocksByDefault(t *testing.T) {
	h := newHarness(t)
	h.server.Close()
	code, stdout, stderr := h.run(preToolUse, claudeLine...)
	if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "Paidwen did not answer") || !strings.Contains(stderr, ", so paidwen check blocks Bash as -on-unavailable block asks.") {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
}

func TestAWrongConfigurationRefusesWithoutAsking(t *testing.T) {
	cases := []struct {
		name   string
		stdin  string
		args   []string
		key    string
		stderr string
	}{
		{
			name:   "a machine without key",
			stdin:  preToolUse,
			args:   claudeLine,
			stderr: "Paidwen refused the action: this machine has no Paidwen key: run paidwen login, or set PAIDWEN_API_KEY.",
		},
		{
			name:   "a pointer that finds nothing",
			stdin:  preToolUse,
			args:   []string{"-tool", "/name", "-params", "/tool_input"},
			key:    secret,
			stderr: "Paidwen refused the action: the pointer /name finds no name of a tool in the input of the hook.",
		},
		{
			name:   "an input that is not JSON",
			stdin:  "PreToolUse Bash",
			args:   claudeLine,
			key:    secret,
			stderr: "Paidwen refused the action: the input of the hook is not JSON: invalid character 'P' looking for beginning of value.",
		},
		{
			name:   "a cost that is not a number",
			stdin:  preToolUse,
			args:   claude("-cost", "/tool_name"),
			key:    secret,
			stderr: "Paidwen refused the action: the pointer /tool_name finds no number of US dollars in the input of the hook.",
		},
		{
			name:   "an unknown flag",
			stdin:  preToolUse,
			args:   claude("-fail-open"),
			key:    secret,
			stderr: "Paidwen refused the action: the flags of paidwen check are wrong: flag provided but not defined: -fail-open.",
		},
		{
			name:   "an unknown choice",
			stdin:  preToolUse,
			args:   claude("-on-unavailable", "pass"),
			key:    secret,
			stderr: `Paidwen refused the action: the flags of paidwen check are wrong: -on-unavailable is "pass", not block or allow.`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.values["PAIDWEN_API_KEY"] = c.key
			h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
				reply(w, h.decision(request, contract.VerdictAllow))
			}
			code, stdout, stderr := h.run(c.stdin, c.args...)
			if code != 2 || stdout != "" || stderr != c.stderr+"\n" {
				t.Fatalf("got %d %q %q", code, stdout, stderr)
			}
			if asked, _ := h.requests(); len(asked) != 0 {
				t.Fatal("a wrong configuration asks Paidwen")
			}
		})
	}
}

func TestCursorReadsTheAnswerFromTheOutputTemplate(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		if command, _ := (*request.Params)["command"].(string); strings.Contains(command, "push") {
			reply(w, ruled(h.decision(request, contract.VerdictBlock), "rule-push", "No force push"))
			return
		}
		reply(w, h.decision(request, contract.VerdictAllow))
	}
	shell := func(command string) string {
		return `{"command":"` + command + `","cwd":"/work","conversation_id":"c-9","hook_event_name":"beforeShellExecution"}`
	}
	type answer struct {
		Permission   string `json:"permission"`
		AgentMessage string `json:"agent_message"`
	}
	read := func(stdin string) answer {
		t.Helper()
		code, stdout, stderr := h.run(stdin, cursorLine...)
		var parsed answer
		if err := json.Unmarshal([]byte(stdout), &parsed); code != 0 || stderr != "" || err != nil {
			t.Fatalf("got %d %q %q", code, stdout, stderr)
		}
		return parsed
	}
	if got := read(shell("ls")); got != (answer{Permission: "allow"}) {
		t.Fatalf("allow = %+v", got)
	}
	if got := read(shell("git push -f origin main")); got != (answer{Permission: "deny", AgentMessage: `Paidwen blocked shell: the rule "No force push" refuses it.`}) {
		t.Fatalf("deny = %+v", got)
	}
	asked, _ := h.requests()
	if tool := objectAt(t, asked[1], "tool"); tool["name"] != "shell" || objectAt(t, asked[1], "context")["session_id"] != "c-9" {
		t.Fatalf("request = %v", asked[1])
	}
	delete(h.values, "PAIDWEN_API_KEY")
	if got := read(shell("ls")); got.Permission != "deny" || !strings.HasPrefix(got.AgentMessage, "Paidwen refused the action: this machine has no Paidwen key") {
		t.Fatalf("a missing key = %+v", got)
	}
	code, stdout, stderr := h.run(shell("ls"), "-tool-name", "shell", "-output", "{{.Missing}}")
	if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "Paidwen refused the action: the -output template fails: ") {
		t.Fatalf("a broken template = %d %q %q", code, stdout, stderr)
	}
}

func TestATemplateFileAnswersInJSONAndStaysSilentOnAllow(t *testing.T) {
	h := newHarness(t)
	h.decide = func(w http.ResponseWriter, request contract.DecisionRequest) {
		if command, _ := (*request.Params)["command"].(string); strings.Contains(command, "push") {
			reply(w, ruled(h.decision(request, contract.VerdictBlock), "rule-push", "No force push"))
			return
		}
		reply(w, h.decision(request, contract.VerdictAllow))
	}
	file := filepath.Join(t.TempDir(), "deny.tmpl")
	answerTemplate := `{{ if not .Allowed }}{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":{{ json .Reason }}}}{{ end }}` + "\n"
	if err := os.WriteFile(file, []byte(answerTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	line := claude("-output-file", file)
	if code, stdout, stderr := h.run(`{"session_id":"s-1","cwd":"/work","tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"exec-1"}`, line...); code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("allow = %d %q %q", code, stdout, stderr)
	}
	code, stdout, stderr := h.run(`{"session_id":"s-1","cwd":"/work","tool_name":"Bash","tool_input":{"command":"git push -f origin main"},"tool_use_id":"exec-2"}`, line...)
	var answer struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &answer); code != 0 || stderr != "" || err != nil {
		t.Fatalf("deny = %d %q %q", code, stdout, stderr)
	}
	if answer.HookSpecificOutput.PermissionDecision != "deny" || answer.HookSpecificOutput.PermissionDecisionReason != `Paidwen blocked Bash: the rule "No force push" refuses it.` {
		t.Fatalf("deny = %+v", answer)
	}
	code, stdout, stderr = h.run(`{"session_id":"s-1","tool_name":"Bash","tool_input":{"command":"ls"}}`, claude("-output-file", file, "-output", "{{.Reason}}")...)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "only one of them") {
		t.Fatalf("both templates = %d %q %q", code, stdout, stderr)
	}
}
