package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The daemon's settings file: NAME=value lines that `factory setup` writes
// and every command reads at start. systemd's EnvironmentFile reads the
// same format, so one file serves both.

// readEnvFile parses an env file. A missing file is empty, not an error.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want NAME=value", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
		}
		m[strings.TrimSpace(k)] = v
	}
	return m, sc.Err()
}

// writeEnvFile replaces path with the given values in the given order,
// readable only by its owner. Empty values are left out.
func writeEnvFile(path, header string, order []string, vals map[string]string) error {
	var b strings.Builder
	b.WriteString(header)
	for _, k := range order {
		if vals[k] == "" {
			continue
		}
		l, err := envLine(k, vals[k])
		if err != nil {
			return err
		}
		b.WriteString(l + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".factory-env-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// envLine is one NAME=value line, quoted where it must be.
func envLine(k, v string) (string, error) {
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s: value has a line break", k)
	}
	if strings.ContainsAny(v, " \t\"'\\#;$`") {
		v = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return k + "=" + v, nil
}

// updateEnvFile sets keys in the settings file and comments out the
// drop keys that have a value, keeping every other line, comments
// included. A linked file is written through to the file it points at,
// keeping its mode. It returns the keys it commented out.
func updateEnvFile(path string, set map[string]string, drop []string) ([]string, error) {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p // replace the file a link points at, not the link
	}
	mode := os.FileMode(0o600)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	var out, off []string
	done := map[string]bool{}
	text := strings.TrimSuffix(string(b), "\n")
	for _, line := range strings.Split(text, "\n") {
		if text == "" {
			break
		}
		t := strings.TrimSpace(line)
		k, v, ok := strings.Cut(t, "=")
		k = strings.TrimSpace(k)
		switch {
		case t == "" || t[0] == '#' || t[0] == ';' || !ok:
		case set[k] != "":
			if done[k] {
				continue // a second value would win over the new one
			}
			done[k] = true
			l, err := envLine(k, set[k])
			if err != nil {
				return nil, err
			}
			line = l
		case slices.Contains(drop, k) && strings.TrimSpace(v) != "":
			line = "# " + line
			if !slices.Contains(off, k) {
				off = append(off, k)
			}
		}
		out = append(out, line)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !done[k] && set[k] != "" {
			l, err := envLine(k, set[k])
			if err != nil {
				return nil, err
			}
			out = append(out, l)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".factory-env-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(strings.Join(out, "\n") + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return off, os.Rename(tmp.Name(), path)
}

// loadEnvFile sets each variable from the file that the environment does
// not already set, so an explicit export always wins.
func loadEnvFile(path string) error {
	vals, err := readEnvFile(path)
	if err != nil || len(vals) == 0 {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		slog.Warn("settings file is readable by other users; run chmod 600 on it", "path", path)
	}
	for k, v := range vals {
		if _, set := os.LookupEnv(k); !set {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}
