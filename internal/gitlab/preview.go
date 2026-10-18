package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/url"
	"strings"

	"github.com/claptimes5/factory/internal/github"
)

// UploadPreview uploads files to the project, as GitLab does for a
// comment's images. An upload's Ref is its secret/filename, which removes
// it (GitLab 17.2 and later).
func (c *Client) UploadPreview(ctx context.Context, _ int, _ string, files []github.File) ([]github.Upload, error) {
	var out []github.Upload
	for _, f := range files {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, err := mw.CreateFormFile("file", f.Name)
		if err != nil {
			return out, err
		}
		if _, err := fw.Write(f.Data); err != nil {
			return out, err
		}
		if err := mw.Close(); err != nil {
			return out, err
		}
		b, err := c.upload(ctx, "POST", c.project()+"/uploads", &body, mw.FormDataContentType())
		if err != nil {
			return out, err
		}
		var r struct {
			URL string `json:"url"` // /uploads/<secret>/<filename>
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return out, err
		}
		ref := strings.TrimPrefix(r.URL, "/uploads/")
		if ref == r.URL || !strings.Contains(ref, "/") {
			return out, fmt.Errorf("gitlab: upload of %s: unexpected url %q", f.Name, r.URL)
		}
		// Relative: it renders in the project's own comments.
		out = append(out, github.Upload{Name: f.Name, URL: r.URL, Ref: ref})
	}
	return out, nil
}

// RemovePreview deletes uploads by their Ref; one already gone is fine.
// GitLab has the call from 17.2, and only for a Maintainer: otherwise it
// is ErrNotRemovable, since asking again won't change the answer.
func (c *Client) RemovePreview(ctx context.Context, _ int, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, "GET", "/version", nil, &v); err != nil {
		return err
	}
	if !atLeast(v.Version, 17, 2) {
		return fmt.Errorf("%w: GitLab %s can't delete uploads (17.2 can); delete them under the project's Settings > General > Uploads", github.ErrNotRemovable, v.Version)
	}
	for _, ref := range refs {
		secret, name, ok := strings.Cut(ref, "/")
		if !ok {
			continue
		}
		err := c.do(ctx, "DELETE", c.project()+"/uploads/"+url.PathEscape(secret)+"/"+url.PathEscape(name), nil, nil)
		var he *HTTPError
		switch {
		case errors.As(err, &he) && he.Status == 403:
			return fmt.Errorf("%w: deleting uploads takes the Maintainer role, which the factory's token doesn't have", github.ErrNotRemovable)
		case err != nil && !github.IsNotFound(err):
			return err
		}
	}
	return nil
}

// atLeast says whether a GitLab version ("17.2.1-ee") is major.minor or later.
func atLeast(version string, major, minor int) bool {
	var ma, mi int
	if _, err := fmt.Sscanf(version, "%d.%d", &ma, &mi); err != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}

// UpsertPRComment edits note id on merge request n, or posts a new one
// when id is 0 or the note is gone, and returns its id.
func (c *Client) UpsertPRComment(ctx context.Context, n int, id int64, body string) (int64, error) {
	if id != 0 {
		err := c.do(ctx, "PUT", fmt.Sprintf("%s/merge_requests/%d/notes/%d", c.project(), n, id), map[string]any{"body": body}, nil)
		if !github.IsNotFound(err) {
			return id, err
		}
	}
	var r struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, "POST", fmt.Sprintf("%s/merge_requests/%d/notes", c.project(), n), map[string]any{"body": body}, &r)
	return r.ID, err
}
