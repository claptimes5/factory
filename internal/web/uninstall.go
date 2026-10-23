package web

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Uninstaller lists what the factory made on its repos and this machine,
// and removes it once a person confirms.
type Uninstaller interface {
	// Plan is what removing the factory would remove, and what a person
	// has to do themselves. It changes nothing. With repo, it's what
	// removing that repo would: its labels, images and files, and the
	// repo from factory.yaml.
	Plan(ctx context.Context, repo string) APIUninstall
	// Remove removes the labels and images, and the data too with data;
	// with repo, that repo's share, and the repo from factory.yaml. An
	// error refuses it as a whole; each item says how it went.
	Remove(ctx context.Context, data bool, repo string) (APIUninstallResult, error)
}

// APIUninstall is the dry run: what would be removed.
type APIUninstall struct {
	// Labels are the factory's labels on each repo, with how many open
	// issues and PRs carry each. Deleting one takes it off all of them.
	Labels []APIUninstallLabel `json:"labels"`
	// Repos are those whose labels couldn't be read, with why.
	Repos []APIUninstallRepo `json:"repos"`
	// Kept are what looks like the factory's but stays, with why: a
	// GitLab group's label, an intake label you made, a runner image
	// not marked as the factory's.
	Kept []APIUninstallKept `json:"kept"`
	// Images are the job images the factory built, old copies included.
	Images []APIUninstallImage `json:"images"`
	// ImagesNote says why no images could be listed, or "".
	ImagesNote string `json:"images_note"`
	// Data is what the factory keeps on this machine; removing it is a
	// separate choice.
	Data APIUninstallData `json:"data"`
	// Manual are the steps the factory can't do for you: the GitHub App,
	// tokens to revoke, its config files.
	Manual []APIUninstallStep `json:"manual"`
	// Listed: the repo asked about is in factory.yaml, and leaves it.
	Listed bool `json:"listed"`
	// Refused says why nothing can be removed now (jobs running), or "".
	Refused string `json:"refused"`
	// Writable says whether this request may remove anything; Reason
	// says why not.
	Writable bool   `json:"writable"`
	Reason   string `json:"reason"`
}

// APIUninstallLabel is one label on one repo.
type APIUninstallLabel struct {
	Repo string `json:"repo"`
	Name string `json:"name"`
	// Open counts the open issues and PRs with it; -1 when it couldn't
	// be counted.
	Open int `json:"open"`
	// OpenCapped: counting stopped at Open, so there are at least that many.
	OpenCapped bool `json:"open_capped"`
}

// APIUninstallKept is something left alone, and why.
type APIUninstallKept struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// APIUninstallRepo is a repo whose labels couldn't be read.
type APIUninstallRepo struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

// APIUninstallImage is one job image.
type APIUninstallImage struct {
	Ref      string `json:"ref"` // its tag, or its ID for an old untagged copy
	ID       string `json:"id"`
	Size     string `json:"size"`
	Dangling bool   `json:"dangling"`
}

// APIUninstallData is the factory's data folder.
type APIUninstallData struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Size   string `json:"size"`
	// What says what removing it here removes; Note what stays and why.
	What string `json:"what"`
	Note string `json:"note"`
	// Blocked says why it can't be removed (it may not be the factory's
	// alone), or "".
	Blocked string `json:"blocked"`
}

// APIUninstallStep is something to do by hand, with a link if it has one.
type APIUninstallStep struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// APIUninstallResult is how each removal went.
type APIUninstallResult struct {
	Done   []APIUninstallDone `json:"done"`
	Manual []APIUninstallStep `json:"manual"`
}

// APIUninstallDone is one removal.
type APIUninstallDone struct {
	What  string `json:"what"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (s *Server) routesUninstall(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/settings/uninstall", s.apiUninstallPlan)
	mux.HandleFunc("POST /api/v1/settings/uninstall", s.apiUninstall)
}

func (s *Server) apiUninstallPlan(w http.ResponseWriter, r *http.Request) {
	if s.Uninstaller == nil {
		apiErr(w, http.StatusNotFound, "removing the factory isn't available in this dashboard")
		return
	}
	p := s.Uninstaller.Plan(r.Context(), r.URL.Query().Get("repo"))
	p.Writable, p.Reason = s.settingsWritable(r)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiUninstall(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	if s.Uninstaller == nil {
		apiErr(w, http.StatusNotFound, "removing the factory isn't available in this dashboard")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
		Data    bool   `json:"data"`
		Repo    string `json:"repo"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The page asks first; this keeps a stray POST from deleting labels.
	if strings.TrimSpace(in.Confirm) != "remove" {
		apiErr(w, http.StatusBadRequest, `send "confirm": "remove" to remove the factory`)
		return
	}
	// It runs to the end even if the page goes away: half a removal
	// helps no one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	res, err := s.Uninstaller.Remove(ctx, in.Data, in.Repo)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	// The running factory still works on a repo that left factory.yaml
	// until it restarts, and would put its labels back.
	if in.Repo != "" && s.Restarter != nil {
		n := s.Restarter.RequestRestart(ctx)
		res.Manual = append([]APIUninstallStep{{Text: "The factory restarts to stop working on " + in.Repo + ". " + RestartMessage(n, s.Restarter.RestartWaitsFor())}}, res.Manual...)
	}
	writeJSON(w, http.StatusOK, res)
}
