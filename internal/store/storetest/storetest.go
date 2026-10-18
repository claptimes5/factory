// Package storetest opens stores for tests from a database migrated once
// per test binary. Migrating a new database runs every migration, which
// under -race costs a large part of a second; copying a migrated file
// costs a millisecond.
package storetest

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/store"
)

var (
	once     sync.Once
	template []byte
	tmplErr  error
)

func migrated() ([]byte, error) {
	once.Do(func() {
		dir, err := os.MkdirTemp("", "storetest-")
		if err != nil {
			tmplErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "migrated.db")
		st, err := store.Open(path)
		if err != nil {
			tmplErr = err
			return
		}
		// Closing the last connection checkpoints the WAL into the file.
		if tmplErr = st.Close(); tmplErr != nil {
			return
		}
		if _, err := os.Stat(path + "-wal"); err == nil {
			tmplErr = errors.New("the template store's WAL wasn't checkpointed on close")
			return
		}
		template, tmplErr = os.ReadFile(path)
	})
	return template, tmplErr
}

// Open opens a store at path, which must not exist yet, as a copy of a
// freshly migrated database, and closes it when the test ends.
func Open(t testing.TB, path string) *store.Store {
	t.Helper()
	b, err := migrated()
	if err != nil {
		t.Fatalf("migrating the template store: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
