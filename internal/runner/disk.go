package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Disk is the free space where something the jobs write is stored.
type Disk struct {
	Where string // for people: "Docker's storage (/var/lib/docker)"
	Free  uint64 // bytes
}

// DiskChecker is a runner that knows where its containers' files go.
type DiskChecker interface {
	// Disks reports the free space of every filesystem a job's containers
	// write to. images are ones the jobs use, for a probe if one is
	// needed; none is pulled. An error means a disk couldn't be measured.
	Disks(ctx context.Context, images []string) ([]Disk, error)
}

// DiskWait bounds each Docker call made to measure a disk: a wedged
// Docker Desktop can leave `docker info` hanging.
var DiskWait = 20 * time.Second

// FreeAt is the free space, for an unprivileged writer, of the filesystem
// holding path.
func FreeAt(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // Bsize is int64 on Linux, uint32 on macOS
}

// WSL reports whether this is Linux under WSL, where the Linux disks are
// files on a Windows drive and can run out with it.
func WSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// Disks finds where Docker keeps images, containers and their writes. For
// an engine on this machine that's its data root, read directly. Docker
// Desktop keeps it in its own VM, and a remote engine (DOCKER_HOST, a
// docker context) on another machine, so for those it asks a container,
// whose / is on that storage.
func (d *Docker) Disks(ctx context.Context, images []string) ([]Disk, error) {
	ictx, cancel := context.WithTimeout(ctx, DiskWait)
	out, err := exec.CommandContext(ictx, d.bin(), "info", "--format", "{{.OperatingSystem}}|{{.Name}}|{{.DockerRootDir}}").Output()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("docker info: %w", err)
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	for len(f) < 3 {
		f = append(f, "")
	}
	osName, name, root := f[0], f[1], f[2]
	desktop := strings.Contains(osName, "Docker Desktop")
	host, _ := os.Hostname()
	if !desktop && root != "" && strings.EqualFold(name, host) {
		if free, err := FreeAt(root); err == nil {
			return []Disk{{Where: "Docker's storage (" + root + ")", Free: free}}, nil
		}
	}
	where := "Docker's disk"
	if desktop {
		where = "Docker Desktop's disk"
	}
	free, err := d.probe(ctx, images)
	if err != nil {
		if isNoSpace(err) {
			// It can't even create the probe: that's the answer.
			return []Disk{{Where: where, Free: 0}}, nil
		}
		return nil, err
	}
	return []Disk{{Where: where, Free: free}}, nil
}

// probe reads the free space of a container's root filesystem, from the
// first of images that is there and has df.
func (d *Docker) probe(ctx context.Context, images []string) (uint64, error) {
	var errs []error
	for _, image := range images {
		if image == "" {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, DiskWait)
		var stderr strings.Builder
		cmd := exec.CommandContext(pctx, d.bin(), "run", "--rm", "--pull", "never", "--network", "none",
			"--label", "factory.job=factory-disk-probe", "--entrypoint", "df", image, "-Pk", "/")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		if err == nil {
			return parseDF(string(out))
		}
		errs = append(errs, fmt.Errorf("%s: %w: %s", image, err, strings.TrimSpace(stderr.String())))
		if isNoSpace(errs[len(errs)-1]) {
			break
		}
	}
	if len(errs) == 0 {
		return 0, errors.New("no image to measure Docker's disk with")
	}
	return 0, fmt.Errorf("measuring Docker's disk: %w", errors.Join(errs...))
}

func isNoSpace(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no space left")
}

// parseDF reads the available kilobytes from `df -Pk` output.
func parseDF(out string) (uint64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	kb, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	return kb * 1024, nil
}
