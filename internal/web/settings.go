package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
)

// The Settings screen edits a curated part of factory.yaml (see
// config.Settings). Saving writes the file; the daemon reads it only at
// start, so changes apply when it restarts.

// Restarter restarts the daemon once its running jobs finish.
type Restarter interface {
	RequestRestart(ctx context.Context) int // returns how many jobs still run
	RestartPending() bool
	Running() int
}

// APISetting is one value the Settings screen may change.
type APISetting struct {
	Path    []string `json:"path"`
	Label   string   `json:"label"`
	Help    string   `json:"help"`
	Kind    string   `json:"kind"`    // number, integer, text, bool, select, list or duration
	Options []string `json:"options"` // a select's choices; "" is the default
	Row     string   `json:"row"`     // the routing entry it belongs to, or ""
	Value   any      `json:"value"`   // null when the file leaves it out
	Default any      `json:"default"` // what applies then; null when it's required
}

// APISettingsGroup is one card of settings.
type APISettingsGroup struct {
	Name   string       `json:"name"`
	Help   string       `json:"help"`
	Fields []APISetting `json:"fields"`
}

// APISettings is what factory.yaml says, and whether it can be changed here.
type APISettings struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	Reason   string `json:"reason"` // why not, when it isn't writable
	// Version changes with the file; a save sends it back, so it can't
	// overwrite changes made since.
	Version string `json:"version"`
	// Changed says the file differs from the one the daemon started with.
	Changed        bool               `json:"changed"`
	RestartPending bool               `json:"restart_pending"`
	RunningJobs    int                `json:"running_jobs"`
	Groups         []APISettingsGroup `json:"groups"`
}

// ConfigVersion is the version the Settings screen gives a config file's
// bytes; set Server.StartedConfig to the one the daemon read.
func ConfigVersion(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// settingsWritable says whether settings may be changed from the browser:
// only behind a password, or on an address no other machine can reach.
func (s *Server) settingsWritable() (bool, string) {
	if s.Login != nil && s.Login.Password != "" {
		return true, ""
	}
	if host, _, err := net.SplitHostPort(s.Listen); err == nil {
		if a, err := netip.ParseAddr(strings.Trim(host, "[]")); host == "localhost" || err == nil && a.Unmap().IsLoopback() {
			return true, ""
		}
	}
	return false, "Changing settings here needs a password: set FACTORY_WEB_PASSWORD (see docs/security.md), or edit factory.yaml."
}

func (s *Server) settings(ctx context.Context) (APISettings, []config.SettingGroup, error) {
	b, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return APISettings{}, nil, err
	}
	groups, err := config.Settings(b)
	if err != nil {
		return APISettings{}, nil, fmt.Errorf("%s: %w", s.ConfigPath, err)
	}
	out := APISettings{Path: s.ConfigPath, Version: ConfigVersion(b), Groups: []APISettingsGroup{}}
	out.Writable, out.Reason = s.settingsWritable()
	out.Changed = s.StartedConfig != "" && s.StartedConfig != out.Version
	if s.Restarter != nil {
		out.RestartPending, out.RunningJobs = s.Restarter.RestartPending(), s.Restarter.Running()
	} else {
		out.RunningJobs, _ = s.Store.CountJobs(ctx, state.Running, "")
	}
	for _, g := range groups {
		ag := APISettingsGroup{Name: g.Name, Help: g.Help, Fields: []APISetting{}}
		for _, f := range g.Settings {
			opts := f.Options
			if opts == nil {
				opts = []string{}
			}
			ag.Fields = append(ag.Fields, APISetting{Path: f.Path, Label: f.Label, Help: f.Help, Kind: f.Kind, Options: opts, Row: f.Row, Value: f.Value, Default: f.Default})
		}
		out.Groups = append(out.Groups, ag)
	}
	return out, groups, nil
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		apiErr(w, http.StatusNotFound, "this control room has no config file to edit")
		return
	}
	out, _, err := s.settings(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if s.ConfigPath == "" {
		apiErr(w, http.StatusNotFound, "this control room has no config file to edit")
		return
	}
	if ok, why := s.settingsWritable(); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	var in struct {
		Changes []config.Change `json:"changes"`
		Version string          `json:"version"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.Changes) == 0 {
		apiErr(w, http.StatusBadRequest, "nothing to change")
		return
	}
	// One save at a time: each reads the file the last one wrote.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	cur, groups, err := s.settings(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.Version != "" && in.Version != cur.Version {
		apiErr(w, http.StatusConflict, "factory.yaml changed since this page loaded it; reload to see what changed")
		return
	}
	var changes []config.Change
	for _, c := range in.Changes {
		f, ok := config.FindSetting(groups, c.Path)
		if !ok {
			apiErr(w, http.StatusBadRequest, strings.Join(c.Path, ".")+" can't be changed here; edit factory.yaml")
			return
		}
		v, err := f.Coerce(c.Value)
		if err != nil {
			apiErr(w, http.StatusBadRequest, err.Error())
			return
		}
		changes = append(changes, config.Change{Path: c.Path, Value: v})
	}
	src, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.Edit(src, changes)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The whole file must still load, as the daemon will load it.
	dir, _ := filepath.Abs(filepath.Dir(s.ConfigPath))
	if _, err := config.ParseIn(out, dir); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := writeAtomic(s.ConfigPath, out); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, _, err := s.settings(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.settingsWritable(); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	if s.Restarter == nil {
		s.act(w, fmt.Errorf("this control room can't restart the factory"), "")
		return
	}
	n := s.Restarter.RequestRestart(r.Context())
	msg := "Restarting now."
	if n > 0 {
		msg = fmt.Sprintf("Restarting once %d running jobs finish. No new jobs start until then.", n)
		if n == 1 {
			msg = "Restarting once the running job finishes. No new jobs start until then."
		}
	}
	s.act(w, nil, msg)
}

// writeAtomic replaces the file at path with b, keeping its mode: a
// reader sees the old file or the new one, never half of one.
func writeAtomic(path string, b []byte) error {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p // replace the file a link points at, not the link
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // gone after the rename; cleans up after a failure
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(fi.Mode().Perm()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
