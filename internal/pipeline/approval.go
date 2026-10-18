package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/store"
)

// A label is an approval only when someone who may approve work added it:
// write access on GitHub, Developer or above on GitLab, or the factory
// itself. Anyone can open an issue on a public repo, and an issue template
// can add labels for its author. What they approved is the issue's text at
// that moment: if the author edits it afterwards, the factory stops until
// someone approves the new text with factory:retry.

// errEdited is an issue changed since it was approved, and errUnapproved
// one with no approval on record.
var (
	errEdited     = errors.New("the issue changed after it was approved")
	errUnapproved = errors.New("no approval is recorded for the issue")
)

// issueHash identifies an issue's text: what an approval approves. Line
// endings and surrounding space don't count, so the same text read
// through another API, or stored with CRLFs, still matches.
func issueHash(is github.Issue) string {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	sum := sha256.Sum256([]byte(norm(is.Title) + "\x00" + norm(is.Body)))
	return hex.EncodeToString(sum[:])
}

// approvedBy is who added label to issue n and the text they approved,
// when they may approve work. When they may not, or the issue changed
// after they labeled it, it takes the label off and says why on the
// issue; when the host can't say, it leaves everything for the next poll.
func (p *Pipeline) approvedBy(ctx context.Context, repo string, n int, label string) (string, github.Issue, bool) {
	gh := p.gh(repo)
	a, err := gh.Approver(ctx, n, label)
	if err != nil {
		p.warn(err, "Approver")
		return "", github.Issue{}, false
	}
	if a.OK && !a.Edited {
		return a.By, a.Issue, true
	}
	by := "someone the factory can't identify"
	if a.By != "" {
		by = "@" + a.By
	}
	msg := fmt.Sprintf("**Factory: not starting.** `%s` was added by %s, who can't approve work here. "+
		"Someone with write access to the repository (Developer or above on GitLab) can add it to approve this issue.", label, by)
	if a.OK {
		msg = fmt.Sprintf("**Factory: not starting.** The issue's title or text changed after %s added `%s`. "+
			"Read the new text; if it's fine, add `%s` again to approve it.", by, label, label)
	}
	p.log().Warn("label isn't an approval", "repo", repo, "issue", n, "label", label, "by", a.By, "edited", a.Edited)
	// Off first, so a failed comment can't make it say this every poll.
	if err := gh.RemoveLabel(ctx, n, label); err != nil {
		p.warn(err, "RemoveLabel")
		return "", github.Issue{}, false
	}
	p.warn(gh.Comment(ctx, n, msg), "Comment")
	return "", github.Issue{}, false
}

// isNew reports whether the factory has no item for the issue yet.
func (p *Pipeline) isNew(ctx context.Context, repo string, n int) (bool, error) {
	_, err := p.Store.ItemByIssue(ctx, repo, n)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// approve records the issue's text as what who approved for the item.
func (p *Pipeline) approve(ctx context.Context, it *store.WorkItem, is github.Issue, who string) error {
	return p.Store.SetApproval(ctx, it.ID, store.Approval{IssueHash: issueHash(is), ApprovedBy: who})
}

// unchanged returns errEdited when the issue isn't the text that was
// approved, and errUnapproved when nothing was. An item from before
// approvals were recorded (an empty hash) takes its text now as approved.
func (p *Pipeline) unchanged(ctx context.Context, it *store.WorkItem, is github.Issue) error {
	a, err := p.Store.ItemApproval(ctx, it.ID)
	if err != nil {
		return err
	}
	switch {
	case a == nil:
		return errUnapproved
	case a.IssueHash == "":
		return p.approve(ctx, it, is, "(approved before the factory recorded approvals)")
	case a.IssueHash != issueHash(is):
		return errEdited
	}
	return nil
}

// parkEdited parks an item whose issue changed after approval, or that
// has none, and reports whether err was one of those.
func (p *Pipeline) parkEdited(ctx context.Context, it *store.WorkItem, err error) bool {
	switch {
	case errors.Is(err, errEdited):
		who := "it was approved"
		if a, _ := p.Store.ItemApproval(ctx, it.ID); a != nil && a.ApprovedBy != "" {
			who = "@" + a.ApprovedBy + " approved it"
		}
		p.warn(p.park(ctx, it, fmt.Sprintf("The issue's title or text changed after %s, so the factory stopped rather than build something nobody approved. "+
			"Read the new text; if it's fine, add `%s` to approve it.", who, LabelRetry)), "park")
	case errors.Is(err, errUnapproved):
		p.warn(p.park(ctx, it, fmt.Sprintf("The factory has no record of who approved this issue, so it stopped. "+
			"Read it; if it's fine, add `%s` to approve it.", LabelRetry)), "park")
	default:
		return false
	}
	return true
}

// notApproved reports whether err means the issue's text isn't approved.
func notApproved(err error) bool { return errors.Is(err, errEdited) || errors.Is(err, errUnapproved) }
