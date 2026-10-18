package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	CancelRestart(ctx context.Context) bool // says whether one was waiting
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
// only behind a password, or from this machine itself. A loopback listener
// can still be published by a proxy (tailscale serve, Caddy), so the
// request must name a localhost address too, and not come through one.
func (s *Server) settingsWritable(r *http.Request) (bool, string) {
	if s.Login != nil && s.Login.Password != "" {
		return true, ""
	}
	if loopback(s.Listen) && (r == nil || loopback(r.Host) && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("Forwarded") == "") {
		return true, ""
	}
	return false, "Changing settings here needs a password: set FACTORY_WEB_PASSWORD (see docs/security.md), or edit factory.yaml."
}

// loopback says whether a host or host:port is this machine only.
func loopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	a, err := netip.ParseAddr(host)
	return strings.EqualFold(host, "localhost") || err == nil && a.Unmap().IsLoopback()
}

func (s *Server) settings(r *http.Request) (APISettings, []config.SettingGroup, []byte, error) {
	ctx := r.Context()
	b, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return APISettings{}, nil, nil, err
	}
	groups, err := config.Settings(b)
	if err != nil {
		return APISettings{}, nil, nil, fmt.Errorf("%s: %w", s.ConfigPath, err)
	}
	out := APISettings{Path: s.ConfigPath, Version: ConfigVersion(b), Groups: []APISettingsGroup{}}
	out.Writable, out.Reason = s.settingsWritable(r)
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
	return out, groups, b, nil
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		apiErr(w, http.StatusNotFound, "this control room has no config file to edit")
		return
	}
	out, _, _, err := s.settings(r)
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
	if ok, why := s.settingsWritable(r); !ok {
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
	res, code, err := s.saveChanges(r, in.Changes, in.Version)
	if err != nil {
		apiErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// saveChanges writes changes to factory.yaml if the file is still the
// version the page loaded, each change is a setting the screen lists and
// fits it, and the result loads. It returns the settings saved, or an
// error with its HTTP status. The caller holds settingsMu.
func (s *Server) saveChanges(r *http.Request, in []config.Change, version string) (APISettings, int, error) {
	cur, groups, src, err := s.settings(r)
	if err != nil {
		return APISettings{}, http.StatusInternalServerError, err
	}
	if version != cur.Version {
		return APISettings{}, http.StatusConflict, errors.New("factory.yaml changed since this page loaded it; reload to see what changed")
	}
	var changes []config.Change
	for _, c := range in {
		f, ok := config.FindSetting(groups, c.Path)
		if !ok {
			return APISettings{}, http.StatusBadRequest, errors.New(strings.Join(c.Path, ".") + " can't be changed here; edit factory.yaml")
		}
		v, err := f.Coerce(c.Value)
		if err != nil {
			return APISettings{}, http.StatusBadRequest, err
		}
		changes = append(changes, config.Change{Path: c.Path, Value: v})
	}
	out, err := config.Edit(src, changes)
	if err != nil {
		return APISettings{}, http.StatusBadRequest, err
	}
	// The whole file must still load, as the daemon will load it.
	dir, _ := filepath.Abs(filepath.Dir(s.ConfigPath))
	if _, err := config.ParseIn(out, dir); err != nil {
		return APISettings{}, http.StatusBadRequest, err
	}
	if err := writeAtomic(s.ConfigPath, out); err != nil {
		return APISettings{}, http.StatusInternalServerError, err
	}
	res, _, _, err := s.settings(r)
	if err != nil {
		return APISettings{}, http.StatusInternalServerError, err
	}
	return res, http.StatusOK, nil
}

func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	if s.Restarter == nil {
		s.act(w, fmt.Errorf("this control room can't restart the factory"), "")
		return
	}
	// A file someone broke by hand since would keep the factory down.
	if err := s.configLoads(); err != nil {
		s.act(w, fmt.Errorf("not restarting: %w", err), "")
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

func (s *Server) apiCancelRestart(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	if s.Restarter == nil || !s.Restarter.CancelRestart(r.Context()) {
		s.act(w, fmt.Errorf("no restart is waiting"), "")
		return
	}
	s.act(w, nil, "Restart called off; jobs are starting again.")
}

// configLoads checks the config file as the daemon will load it.
func (s *Server) configLoads() error {
	if s.ConfigPath == "" {
		return nil
	}
	b, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return err
	}
	dir, _ := filepath.Abs(filepath.Dir(s.ConfigPath))
	if _, err := config.ParseIn(b, dir); err != nil {
		return fmt.Errorf("%s doesn't load: %w", s.ConfigPath, err)
	}
	return nil
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
