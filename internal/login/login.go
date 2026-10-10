package login

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/paidwen/paidwen-cli/internal/api"
	"github.com/paidwen/paidwen-cli/internal/contract"
	"github.com/paidwen/paidwen-cli/internal/credentials"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/state"
)

const (
	defaultInterval = 5 * time.Second
	slowDownStep    = 5 * time.Second
	confirmFor      = 5 * time.Second
	confirmEvery    = 500 * time.Millisecond
	interrupted     = 130
)

var overrides = []string{"PAIDWEN_API_KEY", "PAIDWEN_IDENTITY_TOKEN_FILE"}

type flow struct {
	open func(ctx context.Context, address string) error
	wait func(ctx context.Context, delay time.Duration) error
}

func Login(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return flow{open: openBrowser, wait: sleep}.login(ctx, e, args, stderr)
}

func (f flow) login(ctx context.Context, e env.Env, args []string, stderr io.Writer) int {
	set := flag.NewFlagSet("login", flag.ContinueOnError)
	address := set.String("url", cmp.Or(strings.TrimRight(e.Getenv("PAIDWEN_BASE_URL"), "/"), credentials.DefaultBaseURL), "the address of Paidwen")
	noBrowser := set.Bool("no-browser", false, "print the sign-in address without opening a browser")
	if code, stop := parse(set, "paidwen login [-url address] [-no-browser]", args, stderr); stop {
		return code
	}
	base := strings.TrimRight(*address, "/")
	if !web(base) {
		say(stderr, "paidwen: -url takes an http or https address, not "+strconv.Quote(*address))
		return 2
	}
	for _, name := range overrides {
		if e.Getenv(name) != "" {
			say(stderr, name+" is set, and Paidwen uses it before the key of paidwen login. Clear "+name+", then run paidwen login again.")
			return 1
		}
	}
	var device contract.DeviceAuthorizationResponse
	form := url.Values{"client_id": {string(contract.CommandClientIDPaidwen)}}
	if err := api.PostForm(ctx, e.HTTP, e.UserAgent(), base+"/v1/oauth/device/code", form, &device); err != nil {
		if ctx.Err() != nil {
			return stopped(stderr)
		}
		say(stderr, "Paidwen cannot start the sign-in: "+err.Error())
		return 1
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" || device.ExpiresIn <= 0 {
		say(stderr, "The answer of Paidwen to the sign-in is not the JSON the contract describes.")
		return 1
	}
	say(stderr, "Your one-time code is "+device.UserCode+".")
	say(stderr, "Sign in at "+device.VerificationURI+" with this code, choose your organization and accept.")
	if !*noBrowser {
		page := cmp.Or(device.VerificationURIComplete, device.VerificationURI)
		if err := f.browse(ctx, page); err != nil {
			say(stderr, "The browser did not open: "+err.Error())
			say(stderr, "Open "+page+" in a browser.")
		}
	}
	key, code := f.poll(ctx, e, base, device, stderr)
	if key == "" {
		return code
	}
	agent, err := f.confirm(ctx, e, base, key)
	return keep(ctx, e, credentials.Stored{BaseURL: base, APIKey: key}, agent, err, stderr)
}

func (f flow) browse(ctx context.Context, page string) error {
	if !web(page) {
		return errors.New("the address is not an http or https URL")
	}
	return f.open(ctx, page)
}

func (f flow) poll(ctx context.Context, e env.Env, base string, device contract.DeviceAuthorizationResponse, stderr io.Writer) (string, int) {
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}
	deadline := e.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	form := url.Values{
		"grant_type":  {string(contract.DeviceTokenRequestGrantTypeUrnIetfParamsOauthGrantTypeDeviceCode)},
		"device_code": {device.DeviceCode},
		"client_id":   {string(contract.CommandClientIDPaidwen)},
	}
	answering := true
	for {
		if f.wait(ctx, interval) != nil {
			return "", stopped(stderr)
		}
		var granted contract.DeviceTokenResponse
		err := api.PostForm(ctx, e.HTTP, e.UserAgent(), base+"/v1/oauth/token", form, &granted)
		var refusal *api.TokenError
		switch {
		case err == nil && granted.AccessToken != "":
			return granted.AccessToken, 0
		case err == nil:
			say(stderr, "The answer of Paidwen holds no key.")
			return "", 1
		case ctx.Err() != nil:
			return "", stopped(stderr)
		case errors.As(err, &refusal) && refusal.Status < http.StatusInternalServerError:
			answering = true
			switch refusal.Code {
			case contract.TokenErrorErrorAuthorizationPending:
			case contract.TokenErrorErrorSlowDown:
				interval += slowDownStep
			case contract.TokenErrorErrorAccessDenied:
				say(stderr, "The sign-in was refused.")
				return "", 1
			case contract.TokenErrorErrorExpiredToken:
				return "", expired(stderr)
			default:
				say(stderr, "Paidwen refused the sign-in: "+err.Error())
				return "", 1
			}
		case api.Unavailable(err):
			if answering {
				say(stderr, "Paidwen does not answer, and the sign-in keeps waiting: "+err.Error())
			}
			answering = false
		default:
			say(stderr, "paidwen: "+err.Error())
			return "", 1
		}
		if !e.Now().Before(deadline) {
			return "", expired(stderr)
		}
	}
}

func (f flow) confirm(ctx context.Context, e env.Env, base, key string) (contract.CallerAgent, error) {
	client := api.Client{BaseURL: base, Credentials: credentials.NewKey(key, "paidwen login"), HTTP: e.HTTP, UserAgent: e.UserAgent()}
	deadline := e.Now().Add(confirmFor)
	for {
		var agent contract.CallerAgent
		_, err := client.Send(ctx, http.MethodGet, "/v1/agent", nil, nil, &agent)
		if !unauthorized(err) || !e.Now().Before(deadline) {
			return agent, err
		}
		if waited := f.wait(ctx, confirmEvery); waited != nil {
			return agent, waited
		}
	}
}

func keep(ctx context.Context, e env.Env, stored credentials.Stored, agent contract.CallerAgent, unconfirmed error, stderr io.Writer) int {
	previous, kept, _ := credentials.Load(e)
	if unconfirmed == nil {
		stored.AgentID = agent.ID.String()
		stored.AgentName = agent.Name
		stored.OrganizationID = agent.OrganizationID.String()
	}
	if err := credentials.Save(e, stored); err != nil {
		say(stderr, "This machine cannot keep the key: "+err.Error())
		say(stderr, "Revoke the key in the dashboard: "+keysOf(stored.AgentName)+".")
		return 1
	}
	if unconfirmed != nil {
		say(stderr, "This machine keeps the key, but Paidwen did not confirm it: "+unconfirmed.Error())
		say(stderr, "Run paidwen status to check the key again.")
		if ctx.Err() != nil {
			return interrupted
		}
		return 1
	}
	say(stderr, "Logged in as "+agent.Name+". Paidwen now checks the coding agents of this machine.")
	if agent.Status != contract.AgentStatusActive {
		say(stderr, "The status of this agent is "+string(agent.Status)+".")
	}
	if kept && previous.APIKey != stored.APIKey {
		say(stderr, "The key this machine kept before stays valid until you revoke it in the dashboard: "+keysOf(previous.AgentName)+".")
	}
	return 0
}

func Logout(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if code, stop := parse(flag.NewFlagSet("logout", flag.ContinueOnError), "paidwen logout", args, stderr); stop {
		return code
	}
	stored, _, _ := credentials.Load(e)
	removed, err := credentials.Remove(e)
	if err != nil {
		say(stderr, "paidwen: "+err.Error())
		return 1
	}
	if removed {
		say(stderr, "Logged out. This machine no longer keeps the key of "+cmp.Or(stored.AgentName, "the agent")+".")
		say(stderr, "The key stays valid until you revoke it in the dashboard: "+keysOf(stored.AgentName)+".")
	} else {
		say(stderr, "This machine keeps no Paidwen key, so nothing changed.")
	}
	for _, name := range overrides {
		if e.Getenv(name) != "" {
			say(stderr, name+" is still set, so Paidwen still uses it on this machine.")
		}
	}
	return 0
}

func Status(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if code, stop := parse(flag.NewFlagSet("status", flag.ContinueOnError), "paidwen status", args, stderr); stop {
		return code
	}
	resolved, err := credentials.Resolve(e, state.Store{Dir: e.CacheDir, Now: e.Now})
	if err != nil {
		say(stderr, "paidwen: "+err.Error())
		return 1
	}
	source := resolved.Provider.Source()
	client := api.Client{BaseURL: resolved.BaseURL, Credentials: resolved.Provider, HTTP: e.HTTP, UserAgent: e.UserAgent()}
	var agent contract.CallerAgent
	if _, err := client.Send(ctx, http.MethodGet, "/v1/agent", nil, nil, &agent); err != nil {
		var config *api.ConfigError
		switch {
		case errors.As(err, &config):
			say(stderr, "paidwen: "+err.Error())
		case refused(err):
			say(stderr, "Paidwen refuses the key from "+source+": "+err.Error())
			if resolved.Stored != nil {
				say(stderr, "Run paidwen login to sign in again.")
			}
		default:
			say(stderr, "Paidwen at "+resolved.BaseURL+" failed to show the agent of the key: "+err.Error())
		}
		return 1
	}
	say(stdout, "Base URL: "+resolved.BaseURL)
	say(stdout, "Key source: "+source)
	say(stdout, "Agent: "+agent.Name)
	say(stdout, "Agent ID: "+agent.ID.String())
	say(stdout, "Organization ID: "+agent.OrganizationID.String())
	say(stdout, "Environment: "+string(agent.Environment))
	say(stdout, "Status: "+string(agent.Status))
	return 0
}

func Headers(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if code, stop := parse(flag.NewFlagSet("headers", flag.ContinueOnError), "paidwen headers", args, stderr); stop {
		return code
	}
	resolved, err := credentials.Resolve(e, state.Store{Dir: e.CacheDir, Now: e.Now})
	if err != nil {
		say(stderr, "paidwen: "+err.Error())
		return 1
	}
	authorization, err := resolved.Provider.Authorization(ctx)
	if err != nil {
		say(stderr, "paidwen: "+err.Error())
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(map[string]string{"Authorization": authorization}); err != nil {
		say(stderr, "paidwen: "+err.Error())
		return 1
	}
	return 0
}

func parse(set *flag.FlagSet, usage string, args []string, stderr io.Writer) (int, bool) {
	set.SetOutput(stderr)
	set.Usage = func() {
		say(stderr, "Usage: "+usage)
		set.PrintDefaults()
	}
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if set.NArg() > 0 {
		say(stderr, "paidwen: paidwen "+set.Name()+" takes no argument, not "+strconv.Quote(set.Arg(0)))
		set.Usage()
		return 2, true
	}
	return 0, false
}

func web(address string) bool {
	parsed, err := url.Parse(address)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func unauthorized(err error) bool {
	var problem *api.Error
	return errors.As(err, &problem) && problem.Status == http.StatusUnauthorized
}

func refused(err error) bool {
	var token *api.TokenError
	return unauthorized(err) || errors.As(err, &token) && token.Status < http.StatusInternalServerError
}

func keysOf(agent string) string {
	return "Agents, then " + cmp.Or(agent, "the agent") + ", then Keys"
}

func stopped(stderr io.Writer) int {
	say(stderr, "The sign-in stopped.")
	return interrupted
}

func expired(stderr io.Writer) int {
	say(stderr, "The code expired. Run paidwen login again.")
	return 1
}

func say(w io.Writer, line string) {
	_, _ = io.WriteString(w, line+"\n")
}

func openBrowser(ctx context.Context, address string) error {
	name, args := "xdg-open", []string{address}
	switch runtime.GOOS {
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", address}
	case "darwin":
		name = "open"
	}
	return exec.CommandContext(ctx, name, args...).Run()
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
