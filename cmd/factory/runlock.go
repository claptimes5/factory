package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// runFile marks a folder as in use by a running compare or readiness
// check, so removing the factory from another process sees the work.
const runFile = ".running"

// ourLocks are the locks this process holds now. A restart execs in
// place and keeps the pid, so a lock with our pid that isn't here was
// left by the process before it.
var ourLocks = struct {
	sync.Mutex
	paths map[string]bool
}{paths: map[string]bool{}}

// runLock writes this process's pid into dir until release.
func runLock(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f := filepath.Join(dir, runFile)
	ourLocks.Lock()
	ourLocks.paths[f] = true
	ourLocks.Unlock()
	if err := os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		ourLocks.Lock()
		delete(ourLocks.paths, f)
		ourLocks.Unlock()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = os.Remove(f)
			ourLocks.Lock()
			delete(ourLocks.paths, f)
			ourLocks.Unlock()
		})
	}, nil
}

// lockLive says whether the process that wrote a lock still holds it.
func lockLive(f string) bool {
	b, err := os.ReadFile(f)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return false
	}
	if pid == os.Getpid() {
		ourLocks.Lock()
		defer ourLocks.Unlock()
		return ourLocks.paths[f]
	}
	return pidAlive(pid)
}

// sweepReadiness removes readiness check folders no live process holds:
// left by a check that a crash or a restart cut off.
func sweepReadiness(data string) {
	dirs, _ := filepath.Glob(filepath.Join(data, "readiness-*"))
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && !lockLive(filepath.Join(d, runFile)) {
			_ = os.RemoveAll(d)
		}
	}
}

// liveRuns names the compare runs and readiness checks under data that a
// live process holds. A lock whose process is gone counts as not running.
func liveRuns(data string) []string {
	var out []string
	for _, pattern := range []string{"compare/*/" + runFile, "readiness-*/" + runFile} {
		files, _ := filepath.Glob(filepath.Join(data, pattern))
		for _, f := range files {
			if lockLive(f) {
				kind := "a readiness check"
				if strings.Contains(f, string(filepath.Separator)+"compare"+string(filepath.Separator)) {
					kind = "a compare run (" + filepath.Base(filepath.Dir(f)) + ")"
				}
				out = append(out, kind)
			}
		}
	}
	return out
}

// pidAlive says whether a process with this pid exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
