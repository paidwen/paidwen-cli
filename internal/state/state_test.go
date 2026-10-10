package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type entry struct {
	ActionID string `json:"action_id"`
}

func TestAValueComesBackUntilItExpires(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store := Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	if err := store.Put(Pending, "abc", entry{ActionID: "a-1"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	var got entry
	found, err := store.Get(Pending, "abc", &got)
	if err != nil || !found || got.ActionID != "a-1" {
		t.Fatalf("got %+v, %v, %v", got, found, err)
	}
	now = now.Add(2 * time.Minute)
	found, err = store.Get(Pending, "abc", &got)
	if err != nil || found {
		t.Fatalf("an expired value came back: %v, %v", found, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, Pending, "abc.json")); !os.IsNotExist(err) {
		t.Fatal("the expired file stays")
	}
}

func TestADeletedOrMissingValueIsAbsent(t *testing.T) {
	store := Store{Dir: t.TempDir(), Now: time.Now}
	if err := store.Put(Calls, "k", entry{ActionID: "a"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(Calls, "k"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(Calls, "k"); err != nil {
		t.Fatal(err)
	}
	var got entry
	if found, err := store.Get(Calls, "k", &got); err != nil || found {
		t.Fatalf("got %v, %v", found, err)
	}
}

func TestTheSweepRemovesOldFiles(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store := Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	if err := store.Put(Calls, "old", entry{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(store.Dir, Calls, "old.json")
	stale := now.Add(-72 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(store.Dir, Calls, sweptMark)
	if err := os.Chtimes(mark, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Calls, "new", entry{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the old file stays")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, Calls, "new.json")); err != nil {
		t.Fatal(err)
	}
}
