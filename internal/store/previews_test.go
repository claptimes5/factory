package store

import (
	"context"
	"testing"
)

func TestPreviews(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	if p, err := s.Preview(ctx, 1); p != nil || err != nil {
		t.Fatalf("before any: %+v %v", p, err)
	}
	s.SetPreview(ctx, Preview{ItemID: 1, Repo: "o/r", PRNumber: 7, CommentID: 99, JobID: 3, Uploads: []string{"a.png"}})
	s.SetPreview(ctx, Preview{ItemID: 2, Repo: "o/r", PRNumber: 8})
	if p, _ := s.Preview(ctx, 1); p == nil || p.CommentID != 99 || len(p.Uploads) != 1 || p.PostedAt == "" {
		t.Fatalf("preview %+v", p)
	}
	if up, _ := s.PreviewsUp(ctx); len(up) != 1 || up[0].ItemID != 1 {
		t.Fatalf("up %+v", up)
	}
	// Removing the uploads keeps the comment, so the next build edits it.
	s.SetPreview(ctx, Preview{ItemID: 1, Repo: "o/r", PRNumber: 7, CommentID: 99, JobID: 3})
	if up, _ := s.PreviewsUp(ctx); len(up) != 0 {
		t.Fatalf("up after removal %+v", up)
	}
	if p, _ := s.Preview(ctx, 1); p.CommentID != 99 {
		t.Fatalf("kept %+v", p)
	}
}
