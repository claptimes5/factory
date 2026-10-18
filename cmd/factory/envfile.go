package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
		v := vals[k]
		if v == "" {
			continue
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("%s: value has a line break", k)
		}
		if strings.ContainsAny(v, " \t\"'\\#;$`") {
			v = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
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
