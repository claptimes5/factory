package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/store"
)

// A label is an approval only when someone who may approve work added it:
// write access on GitHub, Developer or above on GitLab, or the factory
// itself. Anyone can open an issue on a public repo, and an issue template
// can add labels for its author. What they approved is the issue's text at
// that moment: if the author edits it afterwards, the factory stops until
// someone approves the new text with factory:retry.

// errEdited is an issue changed since it was approved.
var errEdited = errors.New("the issue changed after it was approved")

// issueHash identifies an issue's text: what an approval approves.
func issueHash(is github.Issue) string {
	sum := sha256.Sum256([]byte(is.Title + "\x00" + is.Body))
	return hex.EncodeToString(sum[:])
}

// approvedBy is who added label to the issue, when they may approve work.
// When they may not, it takes the label off and says why on the issue;
// when GitHub can't say, it leaves everything for the next poll.
func (p *Pipeline) approvedBy(ctx context.Context, repo string, is github.Issue, label string) (string, bool) {
	gh := p.gh(repo)
	who, ok, err := gh.Approver(ctx, is.Number, label)
	if err != nil {
		p.warn(err, "Approver")
		return "", false
	}
	if ok {
		return who, true
	}
	by := "someone the factory can't identify"
	if who != "" {
		by = "@" + who
	}
	p.log().Warn("label from someone who can't approve work", "repo", repo, "issue", is.Number, "label", label, "by", who)
	// Off first, so a failed comment can't make it say this every poll.
	if err := gh.RemoveLabel(ctx, is.Number, label); err != nil {
		p.warn(err, "RemoveLabel")
		return "", false
	}
	p.warn(gh.Comment(ctx, is.Number, fmt.Sprintf("**Factory: not starting.** `%s` was added by %s, who can't approve work here. "+
		"Someone with write access to the repository (Developer or above on GitLab) can add it to approve this issue.", label, by)), "Comment")
	return "", false
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
// approved. An item from before approvals were recorded takes its text
// now as approved.
func (p *Pipeline) unchanged(ctx context.Context, it *store.WorkItem, is github.Issue) error {
	a, err := p.Store.ItemApproval(ctx, it.ID)
	if err != nil {
		return err
	}
	if a == nil {
		return p.approve(ctx, it, is, "(approved before the factory recorded approvals)")
	}
	if a.IssueHash != issueHash(is) {
		return errEdited
	}
	return nil
}

// parkEdited parks an item whose issue changed after approval, and
// reports whether err was that.
func (p *Pipeline) parkEdited(ctx context.Context, it *store.WorkItem, err error) bool {
	if !errors.Is(err, errEdited) {
		return false
	}
	who := "it was approved"
	if a, _ := p.Store.ItemApproval(ctx, it.ID); a != nil {
		who = "@" + a.ApprovedBy + " approved it"
	}
	p.warn(p.park(ctx, it, fmt.Sprintf("The issue's title or text changed after %s, so the factory stopped rather than build something nobody approved. "+
		"Read the new text; if it's fine, add `%s` to approve it.", who, LabelRetry)), "park")
	return true
}
