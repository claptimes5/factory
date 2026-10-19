package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// runFile marks a folder as in use by a running compare or readiness
// check, so removing the factory from another process sees the work.
const runFile = ".running"

// runLock writes this process's pid into dir until release.
func runLock(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f := filepath.Join(dir, runFile)
	if err := os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(f) }, nil
}

// liveRuns names the compare runs and readiness checks under data that a
// live process holds. A lock whose process is gone counts as not running.
func liveRuns(data string) []string {
	var out []string
	for _, pattern := range []string{"compare/*/" + runFile, "readiness-*/" + runFile} {
		files, _ := filepath.Glob(filepath.Join(data, pattern))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pidAlive(pid) {
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
