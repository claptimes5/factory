package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Disk is the free space where something the jobs write is stored.
type Disk struct {
	Where string // for people: "Docker's storage (/var/lib/docker)"
	Free  uint64 // bytes
}

// DiskChecker is a runner that knows where its containers' files go.
type DiskChecker interface {
	// Disks reports the free space of every filesystem a job's containers
	// write to; image is one already pulled, for a probe if one is needed.
	Disks(ctx context.Context, image string) ([]Disk, error)
}

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

// Disks finds where Docker keeps images, containers and their writes:
// a directory on this machine for a native engine, or the disk of Docker
// Desktop's VM, which it reads from inside a container since the host
// can't see it. Under WSL the Windows drive holding the virtual disks
// counts too: WSL's own disk reports its virtual size, not what is left.
func (d *Docker) Disks(ctx context.Context, image string) ([]Disk, error) {
	out, err := exec.CommandContext(ctx, d.bin(), "info", "--format", "{{.OperatingSystem}}|{{.DockerRootDir}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker info: %w", err)
	}
	osName, root, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	var disks []Disk
	free, err := uint64(0), error(nil)
	if !strings.Contains(osName, "Docker Desktop") && root != "" {
		free, err = FreeAt(root)
		if err == nil {
			disks = append(disks, Disk{Where: "Docker's storage (" + root + ")", Free: free})
		}
	}
	if len(disks) == 0 {
		// Docker Desktop, or an engine whose storage this machine can't
		// read: ask a container, whose / is on that storage.
		if free, err = d.probe(ctx, image); err != nil {
			return nil, err
		}
		where := "Docker's disk"
		if strings.Contains(osName, "Docker Desktop") {
			where = "Docker Desktop's disk"
		}
		disks = append(disks, Disk{Where: where, Free: free})
	}
	if WSL() && d.WSLDrive != "" {
		if free, err := FreeAt(d.WSLDrive); err == nil {
			disks = append(disks, Disk{Where: "the Windows drive holding WSL's disks (" + d.WSLDrive + ")", Free: free})
		}
	}
	return disks, nil
}

// probe reads the free space of a container's root filesystem.
func (d *Docker) probe(ctx context.Context, image string) (uint64, error) {
	if image == "" {
		return 0, fmt.Errorf("no image to measure Docker's disk with")
	}
	out, err := exec.CommandContext(ctx, d.bin(), "run", "--rm", "--network", "none", "--entrypoint", "df", image, "-Pk", "/").Output()
	if err != nil {
		return 0, fmt.Errorf("measuring Docker's disk: %w", err)
	}
	return parseDF(string(out))
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
