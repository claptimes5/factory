package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDF(t *testing.T) {
	out := "Filesystem     1024-blocks      Used Available Capacity Mounted on\noverlay         61202244  40123456  17958236      70% /\n"
	if b, err := parseDF(out); err != nil || b != 17958236*1024 {
		t.Fatalf("free = %d %v", b, err)
	}
	if _, err := parseDF("df: /: No such file"); err == nil {
		t.Fatal("want an error for output that isn't df's")
	}
	if b, err := FreeAt(t.TempDir()); err != nil || b == 0 {
		t.Fatalf("FreeAt = %d %v", b, err)
	}
}

// fakeDocker writes a docker that answers info with info and run with run
// (a shell snippet), logging each call.
func fakeDocker(t *testing.T, info, run string) (*Docker, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$1\" in\n  info) echo '" + info + "' ;;\n  run) " + run + " ;;\nesac\n"
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Docker{Bin: bin}, log
}

func TestDockerDisks(t *testing.T) {
	ctx := context.Background()
	host, _ := os.Hostname()
	root := t.TempDir()
	df := `printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 100 50 2048 50%% /\n'`

	// An engine on this machine: its data root, read directly.
	d, log := fakeDocker(t, "Ubuntu 24.04|"+host+"|"+root, df)
	ds, err := d.Disks(ctx, []string{"img"})
	if err != nil || len(ds) != 1 || !strings.Contains(ds[0].Where, root) || ds[0].Free == 0 {
		t.Fatalf("local engine: %+v %v", ds, err)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "run") {
		t.Fatal("probed a disk it could read")
	}

	// Docker Desktop, and an engine on another machine, are asked through a
	// container, from the first image there is, never pulled.
	for _, info := range []string{"Docker Desktop|docker-desktop|/var/lib/docker", "Ubuntu|builder-9|" + root} {
		d, log := fakeDocker(t, info, `case "$*" in *missing*) echo 'No such image' >&2; exit 125 ;; esac; `+df)
		ds, err := d.Disks(ctx, []string{"missing", "img"})
		if err != nil || len(ds) != 1 || ds[0].Free != 2048*1024 {
			t.Fatalf("%s: %+v %v", info, ds, err)
		}
		if b, _ := os.ReadFile(log); !strings.Contains(string(b), "--pull never") {
			t.Fatalf("%s: calls %s", info, b)
		}
	}

	// A disk too full to start the probe is a full disk.
	d, _ = fakeDocker(t, "Docker Desktop|x|/var/lib/docker", `echo 'write /var/lib/docker/x: no space left on device' >&2; exit 125`)
	if ds, err := d.Disks(ctx, []string{"img"}); err != nil || len(ds) != 1 || ds[0].Free != 0 {
		t.Fatalf("full: %+v %v", ds, err)
	}
	// Any other failure is an error, not a reading.
	d, _ = fakeDocker(t, "Docker Desktop|x|/var/lib/docker", `exit 1`)
	if _, err := d.Disks(ctx, []string{"img"}); err == nil {
		t.Fatal("a failed probe should be an error")
	}
}
