package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The factory's copy of the ChatGPT sign-in, and the files around it:
//
//	auth.json       the sign-in
//	auth.json.lock  held while it is compared and replaced
//	auth.json.run   held for a whole run, so one process's run can't
//	                refresh it while another process's run does
//	runs/           one entry per run in progress, so a run cut short
//	                by a crash is cleaned up (and its refresh kept)

// maxJobFile caps what the daemon reads of a file a job could have
// written.
const maxJobFile = 64 << 10

// readJobFile reads name under outDir, a directory the job could write.
// It never follows a link out of outDir, never follows a link at all in
// the last part of name, never blocks on a pipe, and reads only a small
// regular file with one link, so whatever the job put there can't hang,
// exhaust or misdirect the daemon.
func readJobFile(outDir, name string) ([]byte, error) {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink != 1 {
		return nil, fmt.Errorf("%s has %d links", name, sys.Nlink)
	}
	if st.Size() > maxJobFile {
		return nil, fmt.Errorf("%s is too big", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxJobFile+1))
	if err == nil && len(b) > maxJobFile {
		err = fmt.Errorf("%s is too big", name)
	}
	return b, err
}

// authFile is the parts of auth.json the factory looks at.
type authFile struct {
	Tokens struct {
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

func parseAuth(b []byte) (authFile, bool) {
	var a authFile
	return a, json.Unmarshal(b, &a) == nil && a.Tokens.RefreshToken != ""
}

// ValidAuth reports whether b is an auth.json with a ChatGPT sign-in in
// it, so a half-written or emptied file never replaces a working one.
func ValidAuth(b []byte) bool {
	_, ok := parseAuth(b)
	return ok
}

// sameAccount reports whether b is a sign-in to the account of was: a job
// may hand back its refreshed sign-in, not another account's.
func sameAccount(b, was []byte) bool {
	a, ok := parseAuth(b)
	w, wok := parseAuth(was)
	return ok && wok && a.Tokens.AccountID == w.Tokens.AccountID
}

// authSecrets are the strings in a sign-in to keep out of logs.
func authSecrets(b []byte) []string {
	var a struct {
		Key    string         `json:"OPENAI_API_KEY"`
		Tokens map[string]any `json:"tokens"`
	}
	if json.Unmarshal(b, &a) != nil {
		return nil
	}
	var out []string
	if len(a.Key) >= 8 {
		out = append(out, a.Key)
	}
	for _, v := range a.Tokens {
		if s, ok := v.(string); ok && len(s) >= 8 {
			out = append(out, s)
		}
	}
	return out
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func fileSum(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

// SaveAuth replaces the sign-in at name with b, as `factory codex-login`
// does, under the same lock a run's save-back takes.
func SaveAuth(name string, b []byte) error {
	if !ValidAuth(b) {
		return errors.New("not a ChatGPT sign-in (no refresh token in it)")
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	unlock, err := lockFile(ctx, name+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return writeAtomic(name, b)
}

// saveIfUnchanged replaces the sign-in at name with b only if it is still
// the one with digest was: a sign-in saved since (`factory codex-login`)
// is newer than a job's refresh of the old one. It reports whether it
// wrote b.
func saveIfUnchanged(name, was string, b []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	unlock, err := lockFile(ctx, name+".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	if cur, err := fileSum(name); err != nil || cur != was {
		return false, nil
	}
	return true, writeAtomic(name, b)
}

// writeAtomic replaces name with b through a temporary file in the same
// directory, mode 0600.
func writeAtomic(name string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".auth-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// lockFile takes an exclusive lock on name, waiting until ctx ends.
func lockFile(ctx context.Context, name string) (func(), error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runEntry records a run in progress: where its CODEX_HOME is and the
// sign-in it started with. Its file stays locked while the run lasts.
type runEntry struct {
	Out string `json:"out"` // the job's out directory
	Was string `json:"was"` // digest of the sign-in it was given
}

func runsDir(file string) string { return filepath.Join(filepath.Dir(file), "runs") }

// startRun records a run and returns the func that ends the record.
func startRun(file string, e runEntry) (func(), error) {
	dir := runsDir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte(e.Out))
	name := filepath.Join(dir, hex.EncodeToString(h[:8])+".json")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another run uses %s: %w", e.Out, err)
	}
	b, _ := json.Marshal(e)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = os.Remove(name); _ = f.Close() }, nil
}

// finishRun keeps a refreshed sign-in the run left in its CODEX_HOME, if
// the factory's copy is still the one it started with, then deletes that
// CODEX_HOME. It returns the sign-in it read and whether it saved it.
func finishRun(file string, e runEntry, was []byte) (got []byte, saved bool, err error) {
	defer func() { _ = os.RemoveAll(filepath.Join(e.Out, codexHomeDir)) }()
	b, rerr := readJobFile(e.Out, filepath.Join(codexHomeDir, "auth.json"))
	if rerr != nil || digest(b) == e.Was || !sameAccount(b, was) {
		return nil, false, nil
	}
	saved, err = saveIfUnchanged(file, e.Was, b)
	return b, saved, err
}

// recoverRuns finishes the runs a crash cut short: those whose record no
// live process holds.
func recoverRuns(file string) error {
	names, err := filepath.Glob(filepath.Join(runsDir(file), "*.json"))
	if err != nil {
		return err
	}
	master, _ := os.ReadFile(file)
	var errs []error
	for _, name := range names {
		f, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			f.Close()
			continue // still running, in another process
		}
		var e runEntry
		b, _ := io.ReadAll(io.LimitReader(f, maxJobFile))
		if json.Unmarshal(b, &e) == nil && e.Out != "" {
			if _, _, err := finishRun(file, e, master); err != nil {
				errs = append(errs, err)
			}
		}
		_ = os.Remove(name)
		f.Close()
	}
	return errors.Join(errs...)
}
