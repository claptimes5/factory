package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/github"
)

func TestPreviewUploads(t *testing.T) {
	ctx := context.Background()
	const p = "/projects/team%2Fweb"
	uploaded := map[string][]byte{}
	notes := map[int64]string{}
	var deleted []string
	version, forbid := "17.4.1-ee", false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		path := r.URL.EscapedPath()
		switch {
		case r.Method == "GET" && path == "/version":
			json.NewEncoder(w).Encode(map[string]any{"version": version})
		case r.Method == "DELETE" && forbid:
			w.WriteHeader(403)
		case r.Method == "POST" && path == p+"/uploads":
			f, h, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(400)
				return
			}
			b, _ := io.ReadAll(f)
			secret := fmt.Sprintf("s%d", len(uploaded)+1)
			uploaded[secret+"/"+h.Filename] = b
			json.NewEncoder(w).Encode(map[string]any{"url": "/uploads/" + secret + "/" + h.Filename, "markdown": "![x](/uploads/" + secret + "/" + h.Filename + ")"})
		case r.Method == "DELETE" && strings.HasPrefix(path, p+"/uploads/"):
			ref := strings.TrimPrefix(path, p+"/uploads/")
			if _, ok := uploaded[ref]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(uploaded, ref)
			deleted = append(deleted, ref)
			w.WriteHeader(204)
		case r.Method == "POST" && path == p+"/merge_requests/5/notes":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			id := int64(len(notes) + 10)
			notes[id] = in["body"]
			json.NewEncoder(w).Encode(map[string]any{"id": id})
		case r.Method == "PUT" && strings.HasPrefix(path, p+"/merge_requests/5/notes/"):
			var id int64
			fmt.Sscan(strings.TrimPrefix(path, p+"/merge_requests/5/notes/"), &id)
			if _, ok := notes[id]; !ok {
				w.WriteHeader(404)
				return
			}
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			notes[id] = in["body"]
			json.NewEncoder(w).Encode(map[string]any{"id": id})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Project: "team/web", Host: "gitlab.example", BaseURL: srv.URL, Tokens: github.StaticToken("tok")}
	ups, err := c.UploadPreview(ctx, 5, "abc", []github.File{{Name: "after-01-desktop.png", Data: []byte("png1")}, {Name: "after-01-phone.png", Data: []byte("png2")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].URL != "/uploads/s1/after-01-desktop.png" || ups[0].Ref != "s1/after-01-desktop.png" || string(uploaded["s2/after-01-phone.png"]) != "png2" {
		t.Fatalf("uploads %+v %v", ups, uploaded)
	}
	if err := c.RemovePreview(ctx, 5, []string{ups[0].Ref, ups[1].Ref, "s9/gone.png"}); err != nil {
		t.Fatal(err)
	}
	if len(uploaded) != 0 || len(deleted) != 2 {
		t.Fatalf("left %v, deleted %v", uploaded, deleted)
	}
	// A Developer token, or a GitLab before 17.2, can't: that's for good.
	ups, _ = c.UploadPreview(ctx, 5, "abc", []github.File{{Name: "a.png", Data: []byte("x")}})
	forbid = true
	if err := c.RemovePreview(ctx, 5, []string{ups[0].Ref}); !errors.Is(err, github.ErrNotRemovable) || !strings.Contains(err.Error(), "Maintainer") {
		t.Fatalf("403: %v", err)
	}
	forbid, version = false, "17.1.3"
	if err := c.RemovePreview(ctx, 5, []string{ups[0].Ref}); !errors.Is(err, github.ErrNotRemovable) || len(uploaded) != 1 {
		t.Fatalf("old GitLab: %v", err)
	}
	id, err := c.UpsertPRComment(ctx, 5, 0, "one")
	if err != nil {
		t.Fatal(err)
	}
	if id2, err := c.UpsertPRComment(ctx, 5, id, "two"); err != nil || id2 != id || notes[id] != "two" || len(notes) != 1 {
		t.Fatalf("edit in place: %d %v %v", id2, err, notes)
	}
	if id3, err := c.UpsertPRComment(ctx, 5, 999, "three"); err != nil || id3 == 999 || len(notes) != 2 {
		t.Fatalf("recreate: %d %v", id3, err)
	}
}
