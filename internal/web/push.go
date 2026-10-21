package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/claptimes5/factory/internal/push"
	"github.com/claptimes5/factory/internal/store"
)

// Push notifications to the browsers that turn them on: the dashboard
// installed on a phone's home screen, or a desktop browser. The factory
// sends what it would send to FACTORY_NOTIFY_URL (cmd/factory, deliver.go).

// APIPush is whether this factory can push, the key a browser subscribes
// with, and the browsers that have.
type APIPush struct {
	Available bool            `json:"available"`
	PublicKey string          `json:"public_key"`
	Devices   []APIPushDevice `json:"devices"`
}

// APIPushDevice is one browser with push on. ID is a hash of its
// endpoint, so the page can tell whether this browser is one of them
// without the endpoint (a capability) leaving the server.
type APIPushDevice struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	CreatedAt string `json:"created_at"`
	FailedAt  string `json:"failed_at"`
}

// PushID is a device's ID: the hash of its endpoint.
func PushID(endpoint string) string {
	h := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(h[:8])
}

func (s *Server) apiPush(w http.ResponseWriter, r *http.Request) {
	out := APIPush{Devices: []APIPushDevice{}}
	if s.Push != nil {
		out.Available, out.PublicKey = true, s.Push.PublicKey()
	}
	subs, err := s.Store.PushSubscriptions(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range subs {
		out.Devices = append(out.Devices, APIPushDevice{ID: PushID(p.Endpoint), Agent: p.Agent, CreatedAt: p.CreatedAt, FailedAt: p.FailedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

type pushIn struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	// Agent names the device, such as "iPhone" or "Chrome on Linux".
	Agent string `json:"agent"`
}

// readPush reads a subscription the page sends: PushSubscription.toJSON().
func (s *Server) readPush(w http.ResponseWriter, r *http.Request) (pushIn, bool) {
	var in pushIn
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return in, false
	}
	if u, err := url.Parse(in.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		apiErr(w, http.StatusBadRequest, "send the browser's push subscription: its endpoint is an https address")
		return in, false
	}
	return in, true
}

// apiPushSubscribe keeps this browser's subscription; a test push follows
// from the page.
func (s *Server) apiPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if s.Push == nil {
		apiErr(w, http.StatusNotFound, "this factory can't send push notifications")
		return
	}
	in, ok := s.readPush(w, r)
	if !ok {
		return
	}
	if in.Keys.P256dh == "" || in.Keys.Auth == "" {
		apiErr(w, http.StatusBadRequest, "the subscription has no keys")
		return
	}
	agent := strings.TrimSpace(in.Agent)
	if len(agent) > 80 {
		agent = agent[:80]
	}
	if err := s.Store.AddPushSubscription(r.Context(), store.PushSubscription{Endpoint: in.Endpoint, P256dh: in.Keys.P256dh, Auth: in.Keys.Auth, Agent: agent}); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APIActionResult{OK: true, Message: "Notifications are on for this device."})
}

func (s *Server) apiPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
		ID       string `json:"id"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	subs, err := s.Store.PushSubscriptions(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range subs {
		if p.Endpoint == in.Endpoint || (in.ID != "" && PushID(p.Endpoint) == in.ID) {
			if err := s.Store.RemovePushSubscription(r.Context(), p.Endpoint); err != nil {
				apiErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, APIActionResult{OK: true, Message: "Notifications are off for that device."})
}

// apiPushTest sends a test to every device with push on.
func (s *Server) apiPushTest(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if s.Push == nil {
		apiErr(w, http.StatusNotFound, "this factory can't send push notifications")
		return
	}
	ctx := r.Context()
	subs, err := s.Store.PushSubscriptions(ctx)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(subs) == 0 {
		apiErr(w, http.StatusBadRequest, "no device has notifications on yet")
		return
	}
	sent, failed := 0, []string{}
	for _, p := range subs {
		err := s.Push.Send(ctx, push.Subscription{Endpoint: p.Endpoint, P256dh: p.P256dh, Auth: p.Auth},
			push.Message{Title: "Factory", Body: "A test from the dashboard. Notifications work on this device.", URL: "/app/", Tag: "test"}, false)
		switch {
		case errors.Is(err, push.ErrGone):
			_ = s.Store.RemovePushSubscription(ctx, p.Endpoint)
		case err != nil:
			failed = append(failed, p.Agent+": "+err.Error())
			_ = s.Store.PushFailed(ctx, p.Endpoint, store.Now())
		default:
			sent++
			_ = s.Store.PushFailed(ctx, p.Endpoint, "")
		}
	}
	if len(failed) > 0 {
		apiErr(w, http.StatusBadGateway, "Sent to "+strconv.Itoa(sent)+"; failed on "+strings.Join(failed, "; "))
		return
	}
	writeJSON(w, http.StatusOK, APIActionResult{OK: true, Message: "Sent. Check the device."})
}
