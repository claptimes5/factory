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
	"slices"
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
	// RestartWaitsFor names other work a restart waits for, such as "the
	// job image build", or is "".
	RestartWaitsFor() string
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
	Changed        bool `json:"changed"`
	RestartPending bool `json:"restart_pending"`
	// RestartWaitsFor names other work the pending restart waits for,
	// such as "the job image build", or is "".
	RestartWaitsFor string             `json:"restart_waits_for"`
	RunningJobs     int                `json:"running_jobs"`
	Groups          []APISettingsGroup `json:"groups"`
	// Presets are how hard the factory works, least first; Preset is the
	// one the file matches, or "" when its settings were changed by hand.
	Presets []APIPreset `json:"presets"`
	Preset  string      `json:"preset"`
	// Builder is the harness that builds, Reviewer the one that reviews
	// (the same with one), and Harnesses those with a key or sign-in.
	Builder   string   `json:"builder"`
	Reviewer  string   `json:"reviewer"`
	Harnesses []string `json:"harnesses"`
	// Warnings are what the file loads with but is worth a second look.
	Warnings []string `json:"warnings"`
}

// APISettingsCheck is what a save would say, without writing: Error is
// why it would be refused, or "".
type APISettingsCheck struct {
	Error    string   `json:"error"`
	Warnings []string `json:"warnings"`
}

// APIPreset is one rung of how hard the factory works.
type APIPreset struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
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
		out.RestartWaitsFor = s.Restarter.RestartWaitsFor()
	} else {
		out.RunningJobs, _ = s.Store.CountJobs(ctx, state.Running, "")
	}
	out.Presets, out.Harnesses, out.Warnings = []APIPreset{}, []string{}, []string{}
	for _, p := range config.Presets {
		out.Presets = append(out.Presets, APIPreset{Key: p.Key, Name: p.Name, Summary: p.Summary})
	}
	if c, err := config.Parse(b); err == nil {
		out.Builder, out.Reviewer = c.Harnesses()
		out.Preset, _ = config.PresetNow(b)
		out.Warnings = append(out.Warnings, c.Warnings()...)
	}
	if s.Harnesses != nil {
		out.Harnesses = s.Harnesses()
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
	changes, err := coerce(groups, in)
	if err != nil {
		return APISettings{}, http.StatusBadRequest, err
	}
	return s.writeChanges(r, src, changes)
}

// coerce checks each change is a setting the screen lists, and fits it.
func coerce(groups []config.SettingGroup, in []config.Change) ([]config.Change, error) {
	var changes []config.Change
	for _, c := range in {
		f, ok := config.FindSetting(groups, c.Path)
		if !ok {
			return nil, errors.New(strings.Join(c.Path, ".") + " can't be changed here; edit factory.yaml")
		}
		v, err := f.Coerce(c.Value)
		if err != nil {
			return nil, err
		}
		changes = append(changes, config.Change{Path: c.Path, Value: v})
	}
	return changes, nil
}

// apiCheckSettings says what saving changes to factory.yaml as it is now
// would say, as the form is edited, and writes nothing.
func (s *Server) apiCheckSettings(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if s.ConfigPath == "" {
		apiErr(w, http.StatusNotFound, "this control room has no config file to edit")
		return
	}
	var in struct {
		Changes []config.Change `json:"changes"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := APISettingsCheck{Warnings: []string{}}
	_, groups, src, err := s.settings(r)
	if err == nil {
		var changes []config.Change
		if changes, err = coerce(groups, in.Changes); err == nil {
			var c *config.Config
			if c, _, err = s.edited(src, changes); err == nil {
				out.Warnings = append(out.Warnings, c.Warnings()...)
			}
		}
	}
	if err != nil {
		out.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// edited is src, factory.yaml as read, with changes applied, if the whole
// file still loads, as the daemon will load it.
func (s *Server) edited(src []byte, changes []config.Change) (*config.Config, []byte, error) {
	out, err := config.Edit(src, changes)
	if err != nil {
		return nil, nil, err
	}
	dir, _ := filepath.Abs(filepath.Dir(s.ConfigPath))
	c, err := config.ParseIn(out, dir)
	if err != nil {
		return nil, nil, err
	}
	return c, out, nil
}

// writeChanges applies changes to src, factory.yaml as read, and writes
// it if it still loads. The caller holds settingsMu and checked the
// version.
func (s *Server) writeChanges(r *http.Request, src []byte, changes []config.Change) (APISettings, int, error) {
	_, out, err := s.edited(src, changes)
	if err != nil {
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

// apiApplyPreset sets how hard the factory works: a preset's changes,
// made for the harness that builds and the one that reviews (as they are,
// unless the request names others it has a key or sign-in for). Like any
// settings change it applies when the factory restarts.
func (s *Server) apiApplyPreset(w http.ResponseWriter, r *http.Request) {
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
		Key      string `json:"key"`
		Builder  string `json:"builder"`
		Reviewer string `json:"reviewer"`
		Version  string `json:"version"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	cur, _, src, err := s.settings(r)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.Version != cur.Version {
		apiErr(w, http.StatusConflict, "factory.yaml changed since this page loaded it; reload to see what changed")
		return
	}
	if in.Builder == "" {
		in.Builder = cur.Builder
	}
	if in.Reviewer == "" {
		in.Reviewer = cur.Reviewer
	}
	for _, h := range []string{in.Builder, in.Reviewer} {
		if h != "claude" && h != "codex" {
			apiErr(w, http.StatusBadRequest, fmt.Sprintf("no harness %q: claude or codex", h))
			return
		}
		if s.Harnesses != nil && !slices.Contains(cur.Harnesses, h) {
			apiErr(w, http.StatusBadRequest, fmt.Sprintf("%s has no key or sign-in yet: add one on the get-started page first", h))
			return
		}
	}
	c, err := config.Parse(src)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.Key == "" {
		in.Key = cur.Preset
	}
	if in.Key == "" {
		in.Key = "balanced" // set by hand: a harness change starts from the middle
	}
	changes, err := c.PresetChanges(in.Key, in.Builder, in.Reviewer)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, code, err := s.writeChanges(r, src, changes)
	if err != nil {
		apiErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	s.act(w, nil, RestartMessage(n, s.Restarter.RestartWaitsFor()))
}

// RestartMessage says when a restart asked for now happens: n jobs are
// running, and busy names other work it waits for ("" for none).
func RestartMessage(n int, busy string) string {
	var wait []string
	switch {
	case n == 1:
		wait = append(wait, "the running job finishes")
	case n > 1:
		wait = append(wait, fmt.Sprintf("%d running jobs finish", n))
	}
	if busy != "" {
		wait = append(wait, busy+" is done")
	}
	if len(wait) == 0 {
		return "Restarting now."
	}
	return "Restarting once " + strings.Join(wait, " and ") + ". No new jobs start until then."
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
