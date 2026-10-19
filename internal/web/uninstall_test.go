package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type fakeUninstaller struct {
	removed int
	data    bool
	busy    string
}

func (f *fakeUninstaller) Plan(context.Context) APIUninstall {
	return APIUninstall{Labels: []APIUninstallLabel{{Repo: "o/r", Name: "factory:ready", Open: 3}}, Refused: f.busy}
}

func (f *fakeUninstaller) Remove(_ context.Context, data bool) (APIUninstallResult, error) {
	if f.busy != "" {
		return APIUninstallResult{}, errors.New(f.busy)
	}
	f.removed++
	f.data = data
	return APIUninstallResult{Done: []APIUninstallDone{{What: "label factory:ready on o/r", OK: true}}}, nil
}

func TestUninstallNeedsConfirmAndWritable(t *testing.T) {
	t.Parallel()
	base, _, s, _ := settingsSetup(t, "127.0.0.1:8080")
	f := &fakeUninstaller{}
	s.Uninstaller = f
	var p APIUninstall
	getJSON(t, base+"/api/v1/settings/uninstall", &p)
	if !p.Writable || len(p.Labels) != 1 || p.Labels[0].Open != 3 {
		t.Fatalf("plan = %+v", p)
	}
	// A stray POST removes nothing.
	if code, _ := postJSON(t, base+"/api/v1/settings/uninstall", "", `{"data":true}`); code != http.StatusBadRequest || f.removed != 0 {
		t.Fatalf("code = %d, removed %d", code, f.removed)
	}
	code, body := postJSON(t, base+"/api/v1/settings/uninstall", "", `{"confirm":"remove","data":true}`)
	var res APIUninstallResult
	json.Unmarshal([]byte(body), &res)
	if code != http.StatusOK || f.removed != 1 || !f.data || len(res.Done) != 1 || !res.Done[0].OK {
		t.Fatalf("code = %d, body %s", code, body)
	}
	f.busy = "Not now: 2 job(s) are running."
	if code, body := postJSON(t, base+"/api/v1/settings/uninstall", "", `{"confirm":"remove"}`); code != http.StatusConflict || !strings.Contains(body, "2 job(s) are running") {
		t.Fatalf("code = %d, body %s", code, body)
	}
}

func TestUninstallRefusedWithoutAPassword(t *testing.T) {
	t.Parallel()
	base, _, s, _ := settingsSetup(t, "0.0.0.0:8080")
	f := &fakeUninstaller{}
	s.Uninstaller = f
	var p APIUninstall
	getJSON(t, base+"/api/v1/settings/uninstall", &p)
	if p.Writable || p.Reason == "" {
		t.Fatalf("plan = %+v", p)
	}
	if code, _ := postJSON(t, base+"/api/v1/settings/uninstall", "", `{"confirm":"remove"}`); code != http.StatusForbidden || f.removed != 0 {
		t.Fatalf("code = %d, removed %d", code, f.removed)
	}
}
