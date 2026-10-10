package check

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
	"uuid"

	"github.com/paidwen/paidwen-cli/internal/api"
	"github.com/paidwen/paidwen-cli/internal/contract"
	"github.com/paidwen/paidwen-cli/internal/credentials"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/hook"
	"github.com/paidwen/paidwen-cli/internal/pointer"
	"github.com/paidwen/paidwen-cli/internal/state"
)

const (
	callTTL   = 24 * time.Hour
	slowDown  = 5 * time.Second
	waitGrace = 60 * time.Second
)

var sleep = func(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Call struct {
	ActionID  string    `json:"action_id"`
	TraceID   string    `json:"trace_id"`
	SpanID    string    `json:"span_id"`
	StartedAt time.Time `json:"started_at"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
}

type pending struct {
	Call
	ApprovalID string          `json:"approval_id"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Decision   json.RawMessage `json:"decision"`
}

type result struct {
	allowed  bool
	reason   string
	decision json.RawMessage
	approval json.RawMessage
}

type view struct {
	Allowed  bool
	Reason   string
	Decision map[string]any
	Approval map[string]any
}

type options struct {
	spec          hook.Spec
	cost          *pointer.Pointer
	wait          time.Duration
	timeout       time.Duration
	onUnavailable string
	outputText    string
	outputFile    string
	output        *template.Template
}

func Run(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	started := e.Now()
	var o options
	set := flag.NewFlagSet("paidwen check", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	o.register(set)
	err := set.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		set.SetOutput(stderr)
		say(stderr, "Usage: paidwen check [flags], with the input of the hook on stdin")
		set.PrintDefaults()
		return 2
	}
	if err == nil {
		err = o.validate(set.Args())
	}
	if err != nil {
		return o.finish(refusal("Paidwen refused the action: the flags of paidwen check are wrong: "+sentence(err.Error())), stdout, stderr)
	}
	return o.finish(o.check(ctx, e, started, stdin, stderr), stdout, stderr)
}

func (o *options) register(set *flag.FlagSet) {
	o.spec.Register(set)
	set.Func("cost", "the JSON pointer to the cost the agent estimates for the tool, in US dollars", func(text string) error {
		parsed, err := pointer.Parse(text)
		if err != nil {
			return err
		}
		o.cost = &parsed
		return nil
	})
	set.DurationVar(&o.wait, "wait", 50*time.Second, "how long to wait for a human approval, shorter than the timeout of the hook")
	set.DurationVar(&o.timeout, "timeout", 3*time.Second, "how long to wait for each answer of Paidwen")
	set.StringVar(&o.onUnavailable, "on-unavailable", "block", "what to do when Paidwen does not answer: block or allow")
	set.StringVar(&o.outputText, "output", "", "a Go text/template printed on stdout for a hook that reads JSON, with .Allowed, .Reason, .Decision, .Approval and the json function")
	set.StringVar(&o.outputFile, "output-file", "", "the path of a file that holds the -output template, for a shell that strips quotes")
}

func (o *options) parseOutput() error {
	text := o.outputText
	switch {
	case o.outputText != "" && o.outputFile != "":
		return errors.New("give the template with -output or with -output-file, and only one of them")
	case o.outputFile != "":
		path, err := expandHome(o.outputFile)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("the template of -output-file cannot be read: %w", err)
		}
		text = string(content)
	case o.outputText == "":
		return nil
	}
	parsed, err := template.New("output").Funcs(template.FuncMap{"json": encode}).Parse(text)
	if err != nil {
		return err
	}
	o.output = parsed
	return nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~\\") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[1:]), nil
}

func (o *options) validate(rest []string) error {
	if len(rest) > 0 {
		return fmt.Errorf("paidwen check takes no argument, and got %q", strings.Join(rest, " "))
	}
	if err := o.spec.Validate(); err != nil {
		return err
	}
	if err := o.parseOutput(); err != nil {
		return err
	}
	switch {
	case o.onUnavailable != "block" && o.onUnavailable != "allow":
		return fmt.Errorf("-on-unavailable is %q, not block or allow", o.onUnavailable)
	case o.wait < 0:
		return errors.New("-wait is negative")
	case o.timeout <= 0:
		return errors.New("-timeout is not positive")
	}
	return nil
}

func (o *options) estimate(document any) (*float64, error) {
	if o.cost == nil {
		return nil, nil
	}
	found, ok := o.cost.Find(document)
	if !ok || found == nil {
		return nil, nil
	}
	number, isNumber := found.(json.Number)
	if !isNumber {
		return nil, fmt.Errorf("the pointer %s finds no number of US dollars in the input of the hook", o.cost)
	}
	usd, err := number.Float64()
	if err != nil {
		return nil, fmt.Errorf("the pointer %s finds no number of US dollars in the input of the hook", o.cost)
	}
	return &usd, nil
}

func (o *options) check(ctx context.Context, e env.Env, started time.Time, stdin io.Reader, stderr io.Writer) result {
	document, err := hook.Decode(stdin)
	if err != nil {
		return misconfigured(err)
	}
	input, err := o.spec.Read(document)
	if err != nil {
		return misconfigured(err)
	}
	cost, err := o.estimate(document)
	if err != nil {
		return misconfigured(err)
	}
	store := state.Store{Dir: e.CacheDir, Now: e.Now}
	resolved, err := credentials.Resolve(e, store)
	if err != nil {
		return misconfigured(err)
	}
	c := &checker{
		options:  o,
		env:      e,
		store:    store,
		client:   &api.Client{BaseURL: resolved.BaseURL, Credentials: resolved.Provider, HTTP: e.HTTP, UserAgent: e.UserAgent()},
		input:    input,
		cost:     cost,
		identity: resolved.Provider.Identity(),
		source:   resolved.Provider.Source(),
		started:  started,
		stderr:   stderr,
	}
	return c.decide(ctx)
}

func (o *options) finish(r result, stdout, stderr io.Writer) int {
	if o.output == nil {
		if r.allowed {
			return 0
		}
		say(stderr, r.reason)
		return 2
	}
	var buffer bytes.Buffer
	err := o.output.Execute(&buffer, view{Allowed: r.allowed, Reason: r.reason, Decision: object(r.decision), Approval: object(r.approval)})
	if err == nil && strings.TrimSpace(buffer.String()) != "" {
		_, err = stdout.Write(buffer.Bytes())
	}
	if err != nil {
		say(stderr, "Paidwen refused the action: the -output template fails: "+sentence(err.Error()))
		return 2
	}
	return 0
}

type checker struct {
	options  *options
	env      env.Env
	store    state.Store
	client   *api.Client
	input    hook.Input
	cost     *float64
	identity string
	source   string
	started  time.Time
	stderr   io.Writer
}

func (c *checker) decide(ctx context.Context) result {
	key := c.input.Fingerprint(c.identity)
	var held pending
	if found, _ := c.store.Get(state.Pending, key, &held); found {
		var decision contract.Decision
		if json.Unmarshal(held.Decision, &decision) == nil && decision.Approval != nil && held.ApprovalID != "" {
			return c.wait(ctx, key, held, decision)
		}
		_ = c.store.Delete(state.Pending, key)
	}
	return c.ask(ctx, key)
}

func (c *checker) ask(ctx context.Context, key string) result {
	id := uuid.NewV4()
	call := Call{ActionID: id.String(), TraceID: randomHex(16), SpanID: randomHex(8), StartedAt: c.started}
	header := http.Header{}
	header.Set("traceparent", "00-"+call.TraceID+"-"+call.SpanID+"-01")
	var answer json.RawMessage
	var decision contract.Decision
	err := c.send(ctx, http.MethodPost, "/v1/decisions", header, c.request(contract.ActionID(id), call.TraceID), &answer)
	if err == nil {
		err = json.Unmarshal(answer, &decision)
	}
	if err != nil {
		if ctx.Err() == nil && api.Unavailable(err) {
			return c.unavailable(err, call)
		}
		return c.failed(ctx, err)
	}
	switch decision.Verdict {
	case contract.VerdictAllow:
		if decision.Incident == nil {
			return c.allow(call, answer, nil, "")
		}
	case contract.VerdictRequireApproval, contract.VerdictEscalate:
		if decision.Incident == nil && decision.Approval != nil {
			held := pending{Call: call, ApprovalID: decision.Approval.ID.String(), ExpiresAt: deadline(decision, c.env.Now()), Decision: answer}
			return c.wait(ctx, key, held, decision)
		}
	}
	return result{reason: describe(c.input.Tool, decision, ""), decision: answer}
}

func (c *checker) request(id contract.ActionID, traceID string) contract.DecisionRequest {
	params := c.input.Params
	request := contract.DecisionRequest{
		ActionID: id,
		Tool:     contract.Tool{Name: c.input.Tool, Kind: contract.ToolKind(c.input.Kind)},
		Params:   &params,
		Context:  &contract.ActionContext{TraceID: &traceID},
	}
	if c.input.Server != "" {
		server := c.input.Server
		request.Tool.Server = &server
	}
	if c.input.Session != "" {
		session := c.input.Session
		request.Context.SessionID = &session
	}
	if c.cost != nil {
		request.Cost = &contract.Cost{EstimatedUsd: *c.cost}
	}
	return request
}

func (c *checker) wait(ctx context.Context, key string, held pending, decision contract.Decision) result {
	interval := seconds(decision.Approval.Interval)
	onExpiry := contract.OnExpiryDeny
	if decision.Approval.OnExpiry != nil {
		onExpiry = *decision.Approval.OnExpiry
	}
	until := c.started.Add(c.options.wait)
	var last json.RawMessage
	var pause time.Duration
	for first := true; ; first = false {
		if !first {
			if c.env.Now().Add(pause).After(until) {
				return c.hold(key, held, decision, last)
			}
			if err := sleep(ctx, pause); err != nil {
				return c.failed(ctx, err)
			}
		}
		status, answer, err := c.poll(ctx, held.ApprovalID)
		var problem *api.Error
		switch {
		case err == nil:
			last = answer
			if !status.ExpiresAt.IsZero() {
				held.ExpiresAt = status.ExpiresAt
			}
			if status.OnExpiry != "" {
				onExpiry = status.OnExpiry
			}
			switch status.Status {
			case contract.ApprovalStateApproved:
				_ = c.store.Delete(state.Pending, key)
				return c.allow(held.Call, held.Decision, answer, "")
			case contract.ApprovalStateDenied:
				_ = c.store.Delete(state.Pending, key)
				return result{reason: describe(c.input.Tool, decision, status.Status), decision: held.Decision, approval: answer}
			case contract.ApprovalStateExpired:
				return c.expire(key, held, decision, onExpiry, answer)
			}
			interval = max(interval, seconds(status.Interval))
			pause = interval
		case errors.As(err, &problem) && problem.Status == http.StatusTooManyRequests:
			interval += slowDown
			pause = max(interval, problem.RetryAfter)
		case ctx.Err() == nil && api.Unavailable(err):
			pause = interval
		default:
			if errors.As(err, &problem) && problem.Status == http.StatusNotFound {
				_ = c.store.Delete(state.Pending, key)
			}
			failure := c.failed(ctx, err)
			failure.decision = held.Decision
			return failure
		}
		if !c.env.Now().Before(held.ExpiresAt.Add(waitGrace)) {
			return c.expire(key, held, decision, onExpiry, last)
		}
	}
}

func (c *checker) poll(ctx context.Context, id string) (contract.ApprovalStatus, json.RawMessage, error) {
	var answer json.RawMessage
	var status contract.ApprovalStatus
	err := c.send(ctx, http.MethodGet, "/v1/approvals/"+url.PathEscape(id), nil, nil, &answer)
	if err == nil {
		err = json.Unmarshal(answer, &status)
	}
	return status, answer, err
}

func (c *checker) expire(key string, held pending, decision contract.Decision, onExpiry contract.OnExpiry, answer json.RawMessage) result {
	_ = c.store.Delete(state.Pending, key)
	if onExpiry == contract.OnExpiryAllow {
		return c.allow(held.Call, held.Decision, answer, "")
	}
	return result{reason: describe(c.input.Tool, decision, contract.ApprovalStateExpired), decision: held.Decision, approval: answer}
}

func (c *checker) hold(key string, held pending, decision contract.Decision, last json.RawMessage) result {
	if ttl := held.ExpiresAt.Sub(c.env.Now()); ttl > 0 {
		if err := c.store.Put(state.Pending, key, held, ttl); err != nil {
			say(c.stderr, "Paidwen cannot keep the approval on this machine, so the next try asks again: "+sentence(err.Error()))
		}
	}
	reason := "Paidwen holds " + c.input.Tool + ": " + approvalAsked(ruleOf(decision)) + ". The approval " + held.ApprovalID +
		" waits in Paidwen until " + held.ExpiresAt.UTC().Format(time.RFC3339) + ". Call " + c.input.Tool +
		" again later with the same parameters, and it runs once someone approves it."
	return result{reason: reason, decision: held.Decision, approval: last}
}

func (c *checker) allow(call Call, decision, approval json.RawMessage, warning string) result {
	call.DecidedAt = c.env.Now()
	if err := c.store.Put(state.Calls, c.input.CallKey(c.identity), call, callTTL); err != nil {
		say(c.stderr, "Paidwen allows "+c.input.Tool+", but paidwen check cannot keep the call, so paidwen record will not send its result: "+sentence(err.Error()))
	}
	return result{allowed: true, reason: warning, decision: decision, approval: approval}
}

func (c *checker) unavailable(err error, call Call) result {
	cause, detail := c.unreachable(err)
	if c.options.onUnavailable == "allow" {
		warning := cause + ", so paidwen check allows " + c.input.Tool + " as -on-unavailable allow asks." + detail
		say(c.stderr, warning)
		return c.allow(call, nil, nil, warning)
	}
	return refusal(cause + ", so paidwen check blocks " + c.input.Tool + " as -on-unavailable block asks." + detail)
}

func (c *checker) unreachable(err error) (string, string) {
	var problem *api.Error
	var token *api.TokenError
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	switch {
	case errors.As(err, &problem):
		return fmt.Sprintf("Paidwen answered HTTP %d", problem.Status), more(reasonOf(problem.Problem))
	case errors.As(err, &token):
		return fmt.Sprintf("Paidwen answered HTTP %d to the exchange of the platform token", token.Status), more(token.Description)
	case errors.Is(err, context.DeadlineExceeded):
		return "Paidwen did not answer within " + c.options.timeout.String(), ""
	case errors.As(err, &syntax), errors.As(err, &mismatch):
		return "Paidwen gave an answer outside its contract", more(err.Error())
	}
	return "Paidwen did not answer", more(err.Error())
}

func (c *checker) failed(ctx context.Context, err error) result {
	if ctx.Err() != nil {
		return refusal("Paidwen refused " + c.input.Tool + ": paidwen check stopped before Paidwen answered.")
	}
	var problem *api.Error
	var token *api.TokenError
	switch {
	case errors.As(err, &problem) && problem.Status == http.StatusUnauthorized:
		return refusal("Paidwen refused the key from " + c.source + ". Run paidwen login to sign in again.")
	case errors.As(err, &problem) && problem.Status == http.StatusForbidden:
		if reason := reasonOf(problem.Problem); reason != "" {
			return refusal("Paidwen refused the agent: " + sentence(reason))
		}
		return refusal("Paidwen refused the agent with HTTP 403.")
	case errors.As(err, &problem):
		if reason := reasonOf(problem.Problem); reason != "" {
			return refusal("Paidwen refused the request: " + sentence(reason))
		}
		return refusal(fmt.Sprintf("Paidwen refused the request with HTTP %d.", problem.Status))
	case errors.As(err, &token):
		return refusal("Paidwen refused the token of the platform: " + sentence(token.Error()))
	}
	return misconfigured(err)
}

func (c *checker) send(ctx context.Context, method, path string, header http.Header, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.options.timeout)
	defer cancel()
	_, err := c.client.Send(ctx, method, path, header, body, out)
	return err
}

func describe(tool string, decision contract.Decision, answer contract.ApprovalState) string {
	blocked := "Paidwen blocked " + tool + ": "
	rule := ruleOf(decision)
	switch {
	case answer == contract.ApprovalStateDenied:
		return blocked + approvalAsked(rule) + ", and the approver denied it."
	case answer == contract.ApprovalStateExpired:
		return blocked + approvalAsked(rule) + ", and the wait expired without an answer."
	case decision.Incident != nil && rule != "":
		return blocked + rule + " refuses it, and the agent is in quarantine (incident " + decision.Incident.ID.String() + ")."
	case decision.Incident != nil:
		return blocked + "the agent is in quarantine (incident " + decision.Incident.ID.String() + ")."
	case rule != "":
		return blocked + rule + " refuses it."
	case decision.Budget != nil:
		return blocked + "the " + string(decision.Budget.Exceeded) + " budget of the agent is " + dollars(decision.Budget.LimitUsd) +
			", and the agent already spent " + dollars(decision.Budget.SpentUsd) + "."
	case decision.Permission != nil && !decision.Permission.Allowed:
		return blocked + "the permissions of the agent do not allow it."
	}
	return blocked + "Paidwen answered " + string(decision.Verdict) + "."
}

func ruleOf(decision contract.Decision) string {
	switch {
	case decision.RuleName != nil && *decision.RuleName != "":
		return "the rule " + strconv.Quote(*decision.RuleName)
	case decision.RuleID != nil && *decision.RuleID != "":
		return "the rule " + *decision.RuleID
	}
	return ""
}

func approvalAsked(rule string) string {
	if rule == "" {
		return "the action needs an approval"
	}
	return rule + " asks for an approval"
}

func deadline(decision contract.Decision, now time.Time) time.Time {
	from := decision.DecidedAt
	if from.IsZero() {
		from = now
	}
	return from.Add(time.Duration(decision.Approval.ExpiresIn) * time.Second)
}

func seconds(count int32) time.Duration {
	return time.Duration(max(count, 1)) * time.Second
}

func randomHex(size int) string {
	id := make([]byte, size)
	_, _ = rand.Read(id)
	if bytes.Count(id, []byte{0}) == size {
		id[size-1] = 1
	}
	return hex.EncodeToString(id)
}

func reasonOf(problem contract.Problem) string {
	if problem.Detail != nil && *problem.Detail != "" {
		return *problem.Detail
	}
	return problem.Title
}

func dollars(value float64) string {
	text := strconv.FormatFloat(value, 'f', 2, 64)
	if parsed, err := strconv.ParseFloat(text, 64); err != nil || parsed != value {
		text = strconv.FormatFloat(value, 'f', -1, 64)
	}
	return "$" + text
}

func refusal(reason string) result {
	return result{reason: reason}
}

func misconfigured(err error) result {
	return refusal("Paidwen refused the action: " + sentence(err.Error()))
}

func more(text string) string {
	if text == "" {
		return ""
	}
	return " " + sentence(text)
}

func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasSuffix(text, ".") {
		return text
	}
	return text + "."
}

func object(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value
}

func encode(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func say(w io.Writer, line string) {
	_, _ = io.WriteString(w, line+"\n")
}
