package env

import (
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type Env struct {
	Version   string
	Getenv    func(string) string
	Now       func() time.Time
	ConfigDir string
	CacheDir  string
	HTTP      *http.Client
}

func System(version string) (Env, error) {
	config, err := directory("PAIDWEN_CONFIG_DIR", os.UserConfigDir)
	if err != nil {
		return Env{}, err
	}
	cache, err := directory("PAIDWEN_CACHE_DIR", os.UserCacheDir)
	if err != nil {
		return Env{}, err
	}
	return Env{
		Version:   version,
		Getenv:    os.Getenv,
		Now:       time.Now,
		ConfigDir: config,
		CacheDir:  cache,
		HTTP:      &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	}, nil
}

func directory(variable string, base func() (string, error)) (string, error) {
	if chosen := os.Getenv(variable); chosen != "" {
		return chosen, nil
	}
	parent, err := base()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, "paidwen"), nil
}

func (e Env) UserAgent() string {
	return "paidwen-cli/" + e.Version
}
