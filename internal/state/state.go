package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Pending = "pending"
	Calls   = "calls"
	Tokens  = "tokens"

	sweepEvery = time.Hour
	keepAtMost = 48 * time.Hour
	sweptMark  = ".swept"
)

type Store struct {
	Dir string
	Now func() time.Time
}

type envelope struct {
	ExpiresAt time.Time       `json:"expires_at"`
	Value     json.RawMessage `json:"value"`
}

func (s Store) path(kind, key string) string {
	return filepath.Join(s.Dir, kind, key+".json")
}

func (s Store) Put(kind, key string, value any, ttl time.Duration) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body, err := json.Marshal(envelope{ExpiresAt: s.Now().Add(ttl).UTC(), Value: encoded})
	if err != nil {
		return err
	}
	directory := filepath.Join(s.Dir, kind)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, key+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporary.Name())
		return err
	}
	if err := os.Rename(temporary.Name(), s.path(kind, key)); err != nil {
		_ = os.Remove(temporary.Name())
		return err
	}
	s.sweep(directory)
	return nil
}

func (s Store) Get(kind, key string, value any) (bool, error) {
	body, err := os.ReadFile(s.path(kind, key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var stored envelope
	if err := json.Unmarshal(body, &stored); err != nil || !s.Now().Before(stored.ExpiresAt) {
		_ = os.Remove(s.path(kind, key))
		return false, nil
	}
	if err := json.Unmarshal(stored.Value, value); err != nil {
		_ = os.Remove(s.path(kind, key))
		return false, nil
	}
	return true, nil
}

func (s Store) Delete(kind, key string) error {
	err := os.Remove(s.path(kind, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s Store) sweep(directory string) {
	mark := filepath.Join(directory, sweptMark)
	now := s.Now()
	if info, err := os.Stat(mark); err == nil && now.Sub(info.ModTime()) < sweepEvery {
		return
	}
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		return
	}
	_ = os.Chtimes(mark, now, now)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == sweptMark || !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > keepAtMost {
			_ = os.Remove(filepath.Join(directory, name))
		}
	}
}
