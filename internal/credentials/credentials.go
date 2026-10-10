package credentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paidwen/paidwen-cli/internal/api"
	"github.com/paidwen/paidwen-cli/internal/contract"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/state"
)

const (
	DefaultBaseURL = "https://api.paidwen.com"
	renewBefore    = 5 * time.Minute
	fileName       = "credentials.json"
)

var ErrMissing = &api.ConfigError{Err: errors.New("this machine has no Paidwen key: run paidwen login, or set PAIDWEN_API_KEY")}

type Stored struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	AgentID        string `json:"agent_id"`
	AgentName      string `json:"agent_name"`
	OrganizationID string `json:"organization_id"`
}

func Path(e env.Env) string {
	return filepath.Join(e.ConfigDir, fileName)
}

func Load(e env.Env) (Stored, bool, error) {
	body, err := os.ReadFile(Path(e))
	if errors.Is(err, fs.ErrNotExist) {
		return Stored{}, false, nil
	}
	if err != nil {
		return Stored{}, false, err
	}
	var stored Stored
	if err := json.Unmarshal(body, &stored); err != nil {
		return Stored{}, false, errors.New("the file " + Path(e) + " is not the JSON paidwen login writes")
	}
	return stored, stored.APIKey != "", nil
}

func Save(e env.Env, stored Stored) error {
	if err := os.MkdirAll(e.ConfigDir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(e.ConfigDir, fileName+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := temporary.Write(append(body, '\n')); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporary.Name())
		return err
	}
	return os.Rename(temporary.Name(), Path(e))
}

func Remove(e env.Env) (bool, error) {
	err := os.Remove(Path(e))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

type Resolved struct {
	BaseURL  string
	Provider api.Provider
	Stored   *Stored
}

func Resolve(e env.Env, store state.Store) (Resolved, error) {
	base := strings.TrimRight(e.Getenv("PAIDWEN_BASE_URL"), "/")
	secret := e.Getenv("PAIDWEN_API_KEY")
	file := e.Getenv("PAIDWEN_IDENTITY_TOKEN_FILE")
	switch {
	case secret != "" && file != "":
		return Resolved{}, &api.ConfigError{Err: errors.New("PAIDWEN_API_KEY and PAIDWEN_IDENTITY_TOKEN_FILE are both set: keep one of them")}
	case secret != "":
		return Resolved{BaseURL: orDefault(base), Provider: Key{secret: secret, source: "PAIDWEN_API_KEY"}}, nil
	case file != "":
		base = orDefault(base)
		return Resolved{BaseURL: base, Provider: &TokenFile{
			path:      file,
			address:   base + "/v1/oauth/token",
			userAgent: e.UserAgent(),
			client:    e.HTTP,
			store:     store,
			now:       e.Now,
		}}, nil
	}
	stored, found, err := Load(e)
	if err != nil {
		return Resolved{}, &api.ConfigError{Err: err}
	}
	if !found {
		return Resolved{}, ErrMissing
	}
	if base == "" {
		base = strings.TrimRight(stored.BaseURL, "/")
	}
	return Resolved{BaseURL: orDefault(base), Provider: Key{secret: stored.APIKey, source: Path(e)}, Stored: &stored}, nil
}

func orDefault(base string) string {
	if base == "" {
		return DefaultBaseURL
	}
	return base
}

type Key struct {
	secret string
	source string
}

func NewKey(secret, source string) Key {
	return Key{secret: secret, source: source}
}

func (k Key) Authorization(context.Context) (string, error) {
	return "Bearer " + k.secret, nil
}

func (k Key) Invalidate() bool {
	return false
}

func (k Key) Identity() string {
	sum := sha256.Sum256([]byte(k.secret))
	return "key:" + hex.EncodeToString(sum[:16])
}

func (k Key) Source() string {
	return k.source
}

type TokenFile struct {
	path      string
	address   string
	userAgent string
	client    *http.Client
	store     state.Store
	now       func() time.Time

	mutex  sync.Mutex
	cached *cachedToken
}

type cachedToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (t *TokenFile) key() string {
	sum := sha256.Sum256([]byte(t.address + "\x00" + t.path))
	return hex.EncodeToString(sum[:])
}

func (t *TokenFile) Authorization(ctx context.Context) (string, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.cached != nil && t.now().Add(renewBefore).Before(t.cached.ExpiresAt) {
		return "Bearer " + t.cached.Token, nil
	}
	var cached cachedToken
	if found, _ := t.store.Get(state.Tokens, t.key(), &cached); found && t.now().Add(renewBefore).Before(cached.ExpiresAt) {
		t.cached = &cached
		return "Bearer " + cached.Token, nil
	}
	subject, err := os.ReadFile(t.path)
	if err != nil {
		return "", &api.ConfigError{Err: errors.New("the token of the platform cannot be read from PAIDWEN_IDENTITY_TOKEN_FILE: " + err.Error())}
	}
	form := url.Values{
		"grant_type":         {string(contract.TokenExchangeRequestGrantTypeUrnIetfParamsOauthGrantTypeTokenExchange)},
		"subject_token":      {strings.TrimSpace(string(subject))},
		"subject_token_type": {string(contract.TokenExchangeRequestSubjectTokenTypeUrnIetfParamsOauthTokenTypeJwt)},
	}
	var granted contract.TokenExchangeResponse
	if err := api.PostForm(ctx, t.client, t.userAgent, t.address, form, &granted); err != nil {
		return "", err
	}
	fresh := cachedToken{Token: granted.AccessToken, ExpiresAt: t.now().Add(time.Duration(granted.ExpiresIn) * time.Second)}
	t.cached = &fresh
	_ = t.store.Put(state.Tokens, t.key(), fresh, time.Duration(granted.ExpiresIn)*time.Second)
	return "Bearer " + fresh.Token, nil
}

func (t *TokenFile) Invalidate() bool {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.cached = nil
	_ = t.store.Delete(state.Tokens, t.key())
	return true
}

func (t *TokenFile) Identity() string {
	sum := sha256.Sum256([]byte(t.address + "\x00" + t.path))
	return "token:" + hex.EncodeToString(sum[:16])
}

func (t *TokenFile) Source() string {
	return "PAIDWEN_IDENTITY_TOKEN_FILE"
}
