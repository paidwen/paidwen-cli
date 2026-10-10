package login

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paidwen/paidwen-cli/internal/credentials"
	"github.com/paidwen/paidwen-cli/internal/env"
)

const (
	agentID        = "0b6f3c52-8d0e-4e7a-9c55-2f1d1c7f9a10"
	organizationID = "5d2a7e91-3b4c-4f6d-8e1a-9c0b7d6e5f42"
	agentName      = "alice@acme.com"
	userCode       = "WDJB-MJHT"
	deviceCode     = "device-secret"
	issuedKey      = "pw_live_issued"
)

type paidwen struct {
	server    *httptest.Server
	expiresIn int32

	mutex   sync.Mutex
	tokens  []string
	agents  []int
	devices int
	forms   []url.Values
	bearers []string
}

func newPaidwen(t *testing.T, tokens []string, agents []int) *paidwen {
	t.Helper()
	p := &paidwen{expiresIn: 900, tokens: tokens, agents: agents}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/device/code", p.authorize)
	mux.HandleFunc("POST /v1/oauth/token", p.token)
	mux.HandleFunc("GET /v1/agent", p.agent)
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *paidwen) authorize(w http.ResponseWriter, r *http.Request) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.devices++
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != "paidwen" {
		answer(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	answer(w, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          p.server.URL + "/device",
		"verification_uri_complete": p.server.URL + "/device?user_code=" + userCode,
		"expires_in":                p.expiresIn,
		"interval":                  5,
	})
}

func (p *paidwen) token(w http.ResponseWriter, r *http.Request) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if err := r.ParseForm(); err != nil {
		answer(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	p.forms = append(p.forms, r.PostForm)
	switch scripted := shift(&p.tokens); scripted {
	case "key":
		answer(w, http.StatusOK, map[string]string{"access_token": issuedKey, "token_type": "Bearer"})
	case "unavailable":
		answer(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	default:
		answer(w, http.StatusBadRequest, map[string]string{"error": scripted})
	}
}

func (p *paidwen) agent(w http.ResponseWriter, r *http.Request) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.bearers = append(p.bearers, r.Header.Get("Authorization"))
	if status := shift(&p.agents); status != http.StatusOK {
		answer(w, status, map[string]any{"type": "about:blank", "title": "Unauthorized", "status": status, "detail": "The key is unknown."})
		return
	}
	answer(w, http.StatusOK, map[string]string{"id": agentID, "name": agentName, "organization_id": organizationID, "environment": "production", "status": "active"})
}

func (p *paidwen) seen() (int, []url.Values, []string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.devices, slices.Clone(p.forms), slices.Clone(p.bearers)
}

func shift[T any](script *[]T) T {
	value := (*script)[0]
	if len(*script) > 1 {
		*script = (*script)[1:]
	}
	return value
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type machine struct {
	env       env.Env
	now       time.Time
	vars      map[string]string
	waits     []time.Duration
	opened    []string
	browser   error
	interrupt func()
}

func newMachine(t *testing.T, vars map[string]string) *machine {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	directory := t.TempDir()
	m := &machine{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), vars: vars}
	m.env = env.Env{
		Version:   "test",
		Getenv:    func(name string) string { return m.vars[name] },
		Now:       func() time.Time { return m.now },
		ConfigDir: filepath.Join(directory, "config"),
		CacheDir:  filepath.Join(directory, "cache"),
		HTTP:      &http.Client{Transport: transport},
	}
	return m
}

func (m *machine) flow() flow {
	return flow{
		open: func(_ context.Context, address string) error {
			m.opened = append(m.opened, address)
			if m.interrupt != nil {
				m.interrupt()
			}
			return m.browser
		},
		wait: func(ctx context.Context, delay time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			m.waits = append(m.waits, delay)
			m.now = m.now.Add(delay)
			return nil
		},
	}
}

func (m *machine) login(ctx context.Context, args ...string) (int, string) {
	var stderr strings.Builder
	code := m.flow().login(ctx, m.env, args, &stderr)
	return code, stderr.String()
}

func (m *machine) run(t *testing.T, command func(context.Context, env.Env, []string, io.Reader, io.Writer, io.Writer) int, args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := command(t.Context(), m.env, args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (m *machine) save(t *testing.T, stored credentials.Stored) {
	t.Helper()
	if err := credentials.Save(m.env, stored); err != nil {
		t.Fatal(err)
	}
}

func (m *machine) kept(t *testing.T) (credentials.Stored, bool) {
	t.Helper()
	stored, found, err := credentials.Load(m.env)
	if err != nil {
		t.Fatal(err)
	}
	return stored, found
}

func contains(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("%q is missing from:\n%s", part, text)
		}
	}
}

func TestLoginPollsUntilTheSignInIsAcceptedThenKeepsTheCheckedKey(t *testing.T) {
	p := newPaidwen(t, []string{"authorization_pending", "slow_down", "key"}, []int{http.StatusUnauthorized, http.StatusOK})
	m := newMachine(t, nil)
	m.save(t, credentials.Stored{BaseURL: p.server.URL, APIKey: "pw_previous", AgentName: agentName})
	code, stderr := m.login(t.Context(), "-url", p.server.URL+"/")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr,
		"Your one-time code is "+userCode+".",
		"Sign in at "+p.server.URL+"/device with this code",
		"Logged in as "+agentName+". Paidwen now checks the coding agents of this machine.",
		"The key this machine kept before stays valid until you revoke it in the dashboard: Agents, then "+agentName+", then Keys.",
	)
	if want := []string{p.server.URL + "/device?user_code=" + userCode}; !slices.Equal(m.opened, want) {
		t.Fatalf("opened %v, want %v", m.opened, want)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 500 * time.Millisecond}; !slices.Equal(m.waits, want) {
		t.Fatalf("waited %v, want %v", m.waits, want)
	}
	_, forms, bearers := p.seen()
	if len(forms) != 3 {
		t.Fatalf("%d polls, want 3", len(forms))
	}
	for _, form := range forms {
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || form.Get("device_code") != deviceCode || form.Get("client_id") != "paidwen" {
			t.Fatalf("poll %v", form)
		}
	}
	if want := []string{"Bearer " + issuedKey, "Bearer " + issuedKey}; !slices.Equal(bearers, want) {
		t.Fatalf("checks %v, want %v", bearers, want)
	}
	want := credentials.Stored{BaseURL: p.server.URL, APIKey: issuedKey, AgentID: agentID, AgentName: agentName, OrganizationID: organizationID}
	if stored, found := m.kept(t); !found || stored != want {
		t.Fatalf("kept %+v, %v, want %+v", stored, found, want)
	}
}

func TestARefusedSignInKeepsNoKey(t *testing.T) {
	p := newPaidwen(t, []string{"authorization_pending", "access_denied"}, []int{http.StatusOK})
	m := newMachine(t, nil)
	code, stderr := m.login(t.Context(), "-url", p.server.URL, "-no-browser")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "The sign-in was refused.")
	if len(m.opened) != 0 {
		t.Fatalf("the browser opened %v", m.opened)
	}
	if _, found := m.kept(t); found {
		t.Fatal("a refused sign-in keeps a key")
	}
}

func TestAnExpiredCodeEndsTheSignInAndAFailedBrowserPrintsTheAddress(t *testing.T) {
	p := newPaidwen(t, []string{"expired_token"}, []int{http.StatusOK})
	m := newMachine(t, nil)
	m.browser = errors.New("no browser on this machine")
	code, stderr := m.login(t.Context(), "-url", p.server.URL)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr,
		"The browser did not open: no browser on this machine",
		"Open "+p.server.URL+"/device?user_code="+userCode+" in a browser.",
		"The code expired. Run paidwen login again.",
	)
	if _, found := m.kept(t); found {
		t.Fatal("an expired sign-in keeps a key")
	}
}

func TestAnUnavailableServiceIsPolledUntilTheCodeExpires(t *testing.T) {
	p := newPaidwen(t, []string{"unavailable"}, []int{http.StatusOK})
	p.expiresIn = 12
	m := newMachine(t, nil)
	code, stderr := m.login(t.Context(), "-url", p.server.URL, "-no-browser")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "The code expired. Run paidwen login again.")
	if count := strings.Count(stderr, "Paidwen does not answer"); count != 1 {
		t.Fatalf("the outage is told %d times:\n%s", count, stderr)
	}
	if _, forms, _ := p.seen(); len(forms) != 3 {
		t.Fatalf("%d polls, want 3", len(forms))
	}
}

func TestCtrlCStopsTheSignIn(t *testing.T) {
	p := newPaidwen(t, []string{"authorization_pending"}, []int{http.StatusOK})
	m := newMachine(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.interrupt = cancel
	code, stderr := m.login(ctx, "-url", p.server.URL)
	if code != interrupted {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "The sign-in stopped.")
	if _, forms, _ := p.seen(); len(forms) != 0 {
		t.Fatalf("%d polls after Ctrl-C", len(forms))
	}
}

func TestAKeyThatPaidwenDoesNotConfirmIsKeptAndTold(t *testing.T) {
	p := newPaidwen(t, []string{"key"}, []int{http.StatusUnauthorized})
	m := newMachine(t, nil)
	code, stderr := m.login(t.Context(), "-url", p.server.URL, "-no-browser")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "This machine keeps the key, but Paidwen did not confirm it: The key is unknown.", "Run paidwen status to check the key again.")
	if _, _, bearers := p.seen(); len(bearers) != 11 {
		t.Fatalf("%d checks, want 11 over 5 seconds", len(bearers))
	}
	if stored, found := m.kept(t); !found || stored != (credentials.Stored{BaseURL: p.server.URL, APIKey: issuedKey}) {
		t.Fatalf("kept %+v, %v", stored, found)
	}
}

func TestLoginRefusesWhileTheEnvironmentGivesTheKey(t *testing.T) {
	p := newPaidwen(t, []string{"key"}, []int{http.StatusOK})
	m := newMachine(t, map[string]string{"PAIDWEN_API_KEY": "pw_from_environment"})
	code, stderr := m.login(t.Context(), "-url", p.server.URL)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "PAIDWEN_API_KEY is set")
	if devices, _, _ := p.seen(); devices != 0 {
		t.Fatalf("%d sign-ins started", devices)
	}
}

func TestLogoutForgetsTheKeyWithoutRevokingIt(t *testing.T) {
	m := newMachine(t, nil)
	m.save(t, credentials.Stored{BaseURL: credentials.DefaultBaseURL, APIKey: "pw_kept", AgentName: agentName})
	code, _, stderr := m.run(t, Logout)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr,
		"This machine no longer keeps the key of "+agentName+".",
		"The key stays valid until you revoke it in the dashboard: Agents, then "+agentName+", then Keys.",
	)
	if _, found := m.kept(t); found {
		t.Fatal("the key stays on the machine")
	}
	m.vars = map[string]string{"PAIDWEN_API_KEY": "pw_from_environment"}
	code, _, stderr = m.run(t, Logout)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "This machine keeps no Paidwen key, so nothing changed.", "PAIDWEN_API_KEY is still set")
}

func TestStatusShowsTheAgentOfTheKey(t *testing.T) {
	p := newPaidwen(t, nil, []int{http.StatusOK})
	m := newMachine(t, nil)
	m.save(t, credentials.Stored{BaseURL: p.server.URL, APIKey: "pw_kept"})
	code, stdout, stderr := m.run(t, Status)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	want := strings.Join([]string{
		"Base URL: " + p.server.URL,
		"Key source: " + credentials.Path(m.env),
		"Agent: " + agentName,
		"Agent ID: " + agentID,
		"Organization ID: " + organizationID,
		"Environment: production",
		"Status: active",
	}, "\n") + "\n"
	if stdout != want {
		t.Fatalf("got:\n%s\nwant:\n%s", stdout, want)
	}
	if _, _, bearers := p.seen(); !slices.Equal(bearers, []string{"Bearer pw_kept"}) {
		t.Fatalf("checks %v", bearers)
	}
}

func TestStatusFailsWithoutKeyAndWithARefusedKey(t *testing.T) {
	p := newPaidwen(t, nil, []int{http.StatusUnauthorized})
	m := newMachine(t, nil)
	code, _, stderr := m.run(t, Status)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "run paidwen login")
	m.save(t, credentials.Stored{BaseURL: p.server.URL, APIKey: "pw_revoked"})
	code, _, stderr = m.run(t, Status)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "Paidwen refuses the key from "+credentials.Path(m.env)+": The key is unknown.", "Run paidwen login to sign in again.")
}

func TestHeadersPrintTheAuthorizationAsAJSONObject(t *testing.T) {
	m := newMachine(t, nil)
	code, _, stderr := m.run(t, Headers)
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	contains(t, stderr, "run paidwen login")
	m.save(t, credentials.Stored{BaseURL: credentials.DefaultBaseURL, APIKey: "pw_kept"})
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{nil, "Bearer pw_kept"},
		{map[string]string{"PAIDWEN_API_KEY": "pw_from_environment"}, "Bearer pw_from_environment"},
	} {
		m.vars = c.vars
		code, stdout, stderr := m.run(t, Headers)
		var headers map[string]string
		if code != 0 || json.Unmarshal([]byte(stdout), &headers) != nil || len(headers) != 1 || headers["Authorization"] != c.want {
			t.Fatalf("exit %d, %q, %q, want %q", code, stdout, stderr, c.want)
		}
	}
}

func TestTheFlagsAreChecked(t *testing.T) {
	m := newMachine(t, nil)
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"-h"}, 0},
		{[]string{"-unknown"}, 2},
		{[]string{"extra"}, 2},
		{[]string{"-url", "ftp://paidwen.example"}, 2},
	} {
		if code, stderr := m.login(t.Context(), c.args...); code != c.code {
			t.Fatalf("paidwen login %v: exit %d, want %d:\n%s", c.args, code, c.code, stderr)
		}
	}
	if code, _, stderr := m.run(t, Status, "extra"); code != 2 {
		t.Fatalf("paidwen status extra: exit %d:\n%s", code, stderr)
	}
}
