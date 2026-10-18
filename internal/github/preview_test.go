package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeGit is GitHub's git data API for one repo, o/r, in memory.
type fakeGit struct {
	mu       sync.Mutex
	ref      string // the preview branch's commit, "" for none
	trees    map[string][]treeEntry
	commits  map[string]string // commit -> tree
	messages map[string]string
	parents  map[string]int
	n        int
	comments map[int64]string
	created  []string
	// factoryBranch is a branch named factory, which git won't let
	// factory/previews sit beside.
	factoryBranch bool
	truncated     bool // the tree listing is cut short
}

func newFakeGit() *fakeGit {
	return &fakeGit{trees: map[string][]treeEntry{}, commits: map[string]string{}, parents: map[string]int{}, comments: map[int64]string{}, messages: map[string]string{}}
}

func (f *fakeGit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	j := func(v any) { json.NewEncoder(w).Encode(v) }
	id := func(kind string) string { f.n++; return fmt.Sprintf("%s%d", kind, f.n) }
	const ref = "/repos/o/r/git/ref/heads/factory/previews"
	const refs = "/repos/o/r/git/refs/heads/factory/previews"
	switch p := r.URL.Path; {
	case r.Method == "GET" && p == ref:
		if f.ref == "" {
			w.WriteHeader(404)
			return
		}
		j(map[string]any{"object": map[string]any{"sha": f.ref}})
	case r.Method == "POST" && p == "/repos/o/r/git/blobs":
		j(map[string]any{"sha": id("blob")})
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/git/commits/"):
		c := strings.TrimPrefix(p, "/repos/o/r/git/commits/")
		parents := []any{}
		for i := 0; i < f.parents[c]; i++ {
			parents = append(parents, map[string]any{"sha": "p"})
		}
		j(map[string]any{"tree": map[string]any{"sha": f.commits[c]}, "message": f.messages[c], "parents": parents})
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/git/trees/"):
		if r.URL.Query().Get("recursive") != "1" {
			w.WriteHeader(400)
			return
		}
		// With the folders, as GitHub lists them.
		es := append([]treeEntry{{Path: "pr-1", Type: "tree"}}, f.trees[strings.TrimPrefix(p, "/repos/o/r/git/trees/")]...)
		j(map[string]any{"tree": es, "truncated": f.truncated})
	case r.Method == "POST" && p == "/repos/o/r/git/trees":
		b, _ := json.Marshal(in["tree"])
		var es []treeEntry
		json.Unmarshal(b, &es)
		t := id("tree")
		f.trees[t] = es
		j(map[string]any{"sha": t})
	case r.Method == "POST" && p == "/repos/o/r/git/commits":
		c := id("commit")
		f.commits[c] = in["tree"].(string)
		f.parents[c] = len(in["parents"].([]any))
		f.messages[c], _ = in["message"].(string)
		j(map[string]any{"sha": c})
	case r.Method == "GET" && p == "/repos/o/r/branches/factory":
		if !f.factoryBranch {
			w.WriteHeader(404)
			return
		}
		j(map[string]any{"commit": map[string]any{"sha": "f1"}})
	case r.Method == "POST" && p == "/repos/o/r/git/refs":
		if in["ref"] != "refs/heads/factory/previews" || f.ref != "" || f.factoryBranch {
			w.WriteHeader(422)
			return
		}
		f.ref = in["sha"].(string)
	case r.Method == "PATCH" && p == refs:
		if in["force"] != true {
			w.WriteHeader(422)
			return
		}
		f.ref = in["sha"].(string)
	case r.Method == "DELETE" && p == refs:
		f.ref = ""
		w.WriteHeader(204)
	case r.Method == "POST" && strings.HasPrefix(p, "/repos/o/r/issues/") && strings.HasSuffix(p, "/comments"):
		n := int64(len(f.created) + 1)
		f.comments[n] = in["body"].(string)
		f.created = append(f.created, p)
		j(map[string]any{"id": n})
	case r.Method == "PATCH" && strings.HasPrefix(p, "/repos/o/r/issues/comments/"):
		var n int64
		fmt.Sscan(strings.TrimPrefix(p, "/repos/o/r/issues/comments/"), &n)
		if _, ok := f.comments[n]; !ok {
			w.WriteHeader(404)
			return
		}
		f.comments[n] = in["body"].(string)
		j(map[string]any{"id": n})
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGit) files() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ref == "" {
		return nil
	}
	var out []string
	for _, e := range f.trees[f.commits[f.ref]] {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}

func TestPreviewBranch(t *testing.T) {
	ctx := context.Background()
	f := newFakeGit()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	ups, err := c.UploadPreview(ctx, 5, "abc123", []File{{"after-01-desktop.png", []byte("png")}, {"after-01-phone.png", []byte("png")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Ref != "pr-5/abc123/after-01-desktop.png" ||
		ups[0].URL != "https://github.com/o/r/blob/factory/previews/pr-5/abc123/after-01-desktop.png?raw=true" {
		t.Fatalf("uploads %+v", ups)
	}
	six, err := c.UploadPreview(ctx, 6, "def", []File{{"after-01-desktop.png", []byte("png")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.files(), ","); got != "pr-5/abc123/after-01-desktop.png,pr-5/abc123/after-01-phone.png,pr-6/def/after-01-desktop.png" {
		t.Fatalf("branch holds %s", got)
	}
	if f.parents[f.ref] != 0 {
		t.Fatal("the preview commit has a parent: removed images would stay in its history")
	}
	// PR 5 closes: its images go, PR 6's stay.
	if err := c.RemovePreview(ctx, 5, []string{ups[0].Ref, ups[1].Ref}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.files(), ","); got != "pr-6/def/after-01-desktop.png" {
		t.Fatalf("after removing PR 5: %s", got)
	}
	if err := c.RemovePreview(ctx, 6, []string{six[0].Ref}); err != nil {
		t.Fatal(err)
	}
	if f.ref != "" {
		t.Fatal("an empty preview branch was kept")
	}
	if err := c.RemovePreview(ctx, 6, []string{six[0].Ref}); err != nil {
		t.Fatalf("removing again: %v", err)
	}
}

func TestUpsertPRComment(t *testing.T) {
	ctx := context.Background()
	f := newFakeGit()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	id, err := c.UpsertPRComment(ctx, 5, 0, "one")
	if err != nil || id != 1 {
		t.Fatalf("create: %d %v", id, err)
	}
	if id, err = c.UpsertPRComment(ctx, 5, id, "two"); err != nil || id != 1 || f.comments[1] != "two" || len(f.created) != 1 {
		t.Fatalf("edit in place: %d %v %+v", id, err, f.comments)
	}
	// Deleted by someone: a new one is posted.
	delete(f.comments, 1)
	if id, err = c.UpsertPRComment(ctx, 5, 1, "three"); err != nil || id == 1 || len(f.created) != 2 {
		t.Fatalf("recreate: %d %v", id, err)
	}
}

// A factory/previews branch that isn't the factory's own is never
// rewritten or deleted, and a branch named factory is named as the reason
// it can't be made.
func TestPreviewBranchLeavesOthersAlone(t *testing.T) {
	ctx := context.Background()
	f := newFakeGit()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
	for name, setup := range map[string]func(){
		"has a parent":  func() { f.commits["h1"], f.parents["h1"], f.messages["h1"] = "t0", 1, previewMessage },
		"other message": func() { f.commits["h1"], f.parents["h1"], f.messages["h1"] = "t0", 0, "wip" },
	} {
		setup()
		f.ref = "h1"
		if _, err := c.UploadPreview(ctx, 5, "k", []File{{"a.png", []byte("x")}}); err == nil || !strings.Contains(err.Error(), "left alone") {
			t.Fatalf("%s: upload: %v", name, err)
		}
		// For good: the factory stops trying and says so.
		if err := c.RemovePreview(ctx, 5, []string{"x"}); !errors.Is(err, ErrNotRemovable) || f.ref != "h1" {
			t.Fatalf("%s: remove: %v, ref %q", name, err, f.ref)
		}
	}
	// A branch too big to list in one go can't be rewritten either.
	f.commits["h1"], f.parents["h1"], f.messages["h1"], f.truncated = "t0", 0, previewMessage, true
	if _, err := c.UploadPreview(ctx, 5, "k", []File{{"a.png", []byte("x")}}); err == nil || !strings.Contains(err.Error(), "too many files") {
		t.Fatalf("truncated: upload: %v", err)
	}
	if err := c.RemovePreview(ctx, 5, []string{"x"}); !errors.Is(err, ErrNotRemovable) || f.ref != "h1" {
		t.Fatalf("truncated: remove: %v, ref %q", err, f.ref)
	}
	f.truncated = false
	f.ref, f.factoryBranch = "", true
	if _, err := c.UploadPreview(ctx, 5, "k", []File{{"a.png", []byte("x")}}); err == nil || !strings.Contains(err.Error(), "a branch named factory") {
		t.Fatalf("factory branch: %v", err)
	}
}
