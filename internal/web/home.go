package web

import (
	"net/http"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Home's "since you looked": when Home was last looked at, kept by the
// factory so a phone and a desk agree, and what went live since.

const homeSeenKey = "home_seen"

// APIHome is what Home shows beside the board.
type APIHome struct {
	// SeenAt is when someone last marked Home seen; "" before the first time.
	SeenAt string `json:"seen_at"`
	// WentLive are the items done since then, newest first; none before the first time.
	WentLive []APIItem `json:"went_live"`
}

func (s *Server) apiHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	seen, err := s.Store.KV(ctx, homeSeenKey)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := APIHome{SeenAt: seen, WentLive: []APIItem{}}
	if seen != "" {
		done, err := s.Store.ListItems(ctx, state.Done)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		done = slices.DeleteFunc(done, func(it *store.WorkItem) bool { return it.UpdatedAt <= seen })
		slices.SortFunc(done, func(a, b *store.WorkItem) int { return strings.Compare(b.UpdatedAt, a.UpdatedAt) })
		for _, it := range done[:min(len(done), 20)] {
			out.WentLive = append(out.WentLive, s.apiItem(ctx, it))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// apiHomeSeen marks Home seen now: the catch-up starts again from here.
func (s *Server) apiHomeSeen(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	now := store.Now()
	if err := s.Store.SetKV(r.Context(), homeSeenKey, now); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APIHome{SeenAt: now, WentLive: []APIItem{}})
}
