package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
)

// DiskEvery is how often the free disk space is measured again.
var DiskEvery = time.Minute

// ErrDiskLow is a drain that stopped with jobs queued for lack of disk.
var ErrDiskLow = errors.New("low on disk")

// diskLow says why there isn't room to start a job, or "". Every job
// writes checkouts and logs to the data directory and an image layer
// where Docker keeps containers, and a full disk fails jobs halfway,
// often while installing dependencies, which then reads as the builder's
// fault. It measures at most once per DiskEvery, and only when a job is
// waiting. Once low, jobs resume only with some room to spare, so a disk
// hovering at the limit doesn't stop and start them with every job.
func (p *Pipeline) diskLow(ctx context.Context) string {
	min := p.Cfg.Budgets.MinFreeDiskGB
	if min <= 0 {
		return ""
	}
	p.diskMu.Lock()
	at, prev := p.diskAt, p.diskMsg
	p.diskMu.Unlock()
	if !at.IsZero() && time.Since(at) < DiskEvery {
		return prev
	}
	if n, err := p.Store.CountJobs(ctx, state.Queued, ""); err == nil && n == 0 {
		return prev // nothing to start: no need to measure
	}
	need := min * 1e9
	if prev != "" {
		need += max(2e9, need/10)
	}
	disks, err := p.disks(ctx)
	msg := ""
	var low []string
	for _, d := range disks {
		if float64(d.Free) < need {
			low = append(low, fmt.Sprintf("%s has %s free", d.Where, gb(d.Free)))
		}
	}
	switch {
	case len(low) > 0:
		msg = fmt.Sprintf("Low on disk: %s. No new job starts until each has %g GB free (budgets.min_free_disk_gb).", strings.Join(low, "; "), min)
	case err != nil && prev != "":
		// Docker couldn't be measured: no reason to think there's room now.
		msg = prev
	}
	if err != nil {
		p.warn(err, "measuring the disk")
	}
	p.diskMu.Lock()
	p.diskAt, p.diskMsg = time.Now(), msg
	p.diskMu.Unlock()
	return msg
}

// disks measures the data directory, Docker's storage, and under WSL the
// Windows drive under WSL's virtual disks, which inside WSL report their
// virtual size rather than what's left. An error is a disk it couldn't
// measure; the others are still returned.
func (p *Pipeline) disks(ctx context.Context) ([]runner.Disk, error) {
	var disks []runner.Disk
	var errs []error
	data := p.Cfg.Path(p.Cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		errs = append(errs, err)
	} else if free, err := freeAt(data); err != nil {
		errs = append(errs, err)
	} else {
		disks = append(disks, runner.Disk{Where: "the data directory (" + data + ")", Free: free})
	}
	if dc, ok := p.Runner.(runner.DiskChecker); ok {
		ds, err := dc.Disks(ctx, p.jobImages())
		disks = append(disks, ds...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if drive := p.Cfg.Runner.WSLDrive; drive != "" && runner.WSL() {
		if free, err := freeAt(drive); err == nil {
			disks = append(disks, runner.Disk{Where: "the Windows drive holding WSL's disks (" + drive + ")", Free: free})
		} else {
			errs = append(errs, err)
		}
	}
	return disks, errors.Join(errs...)
}

// jobImages are the images jobs run in, the repos' own first.
func (p *Pipeline) jobImages() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range append(p.Cfg.RepoNames(), "") {
		img := p.Cfg.Runner.Image
		if r != "" {
			img = p.repoConf(r).Image
		}
		if img != "" && !seen[img] {
			seen[img] = true
			out = append(out, img)
		}
	}
	return out
}

// freeAt measures a host directory; tests replace it.
var freeAt = runner.FreeAt

// DiskLow is why jobs aren't starting for lack of disk, as last measured,
// or "".
func (p *Pipeline) DiskLow() string {
	p.diskMu.Lock()
	defer p.diskMu.Unlock()
	return p.diskMsg
}

func gb(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
