package pipeline

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// DiskEvery is how often the free disk space is measured again.
var DiskEvery = time.Minute

// diskLow says why there isn't room to start a job, or "". Every job
// writes checkouts and logs to the data directory and an image layer
// where Docker keeps containers, and a full disk fails jobs halfway,
// often while installing dependencies, which then reads as the builder's
// fault. It measures at most once per DiskEvery.
func (p *Pipeline) diskLow(ctx context.Context) string {
	min := p.Cfg.Budgets.MinFreeDiskGB
	if min <= 0 {
		return ""
	}
	p.diskMu.Lock()
	if !p.diskAt.IsZero() && time.Since(p.diskAt) < DiskEvery {
		defer p.diskMu.Unlock()
		return p.diskMsg
	}
	p.diskMu.Unlock()

	var disks []runner.Disk
	data := p.Cfg.Path(p.Cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err == nil {
		if free, err := runner.FreeAt(data); err == nil {
			disks = append(disks, runner.Disk{Where: "the data directory (" + data + ")", Free: free})
		}
	}
	if dc, ok := p.Runner.(runner.DiskChecker); ok {
		ds, err := dc.Disks(ctx, p.Cfg.Runner.Image)
		// Not knowing isn't a reason to stop: the jobs would say if Docker is down.
		p.warn(err, "measuring Docker's disk")
		disks = append(disks, ds...)
	}
	var low []string
	for _, d := range disks {
		if float64(d.Free) < min*1e9 {
			low = append(low, fmt.Sprintf("%s has %s free", d.Where, gb(d.Free)))
		}
	}
	msg := ""
	if len(low) > 0 {
		msg = fmt.Sprintf("Low on disk: %s. No new job starts until each has %g GB free (budgets.min_free_disk_gb).", strings.Join(low, "; "), min)
	}
	p.diskMu.Lock()
	p.diskAt, p.diskMsg = time.Now(), msg
	p.diskMu.Unlock()
	return msg
}

// DiskLow is why jobs aren't starting for lack of disk, as last measured,
// or "".
func (p *Pipeline) DiskLow() string {
	p.diskMu.Lock()
	defer p.diskMu.Unlock()
	return p.diskMsg
}

func gb(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
