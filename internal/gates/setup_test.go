package gates

import (
	"strings"
	"testing"
)

// A long failure keeps its start, where the error is, and its end.
func TestExcerptKeepsTheError(t *testing.T) {
	out := "bin/rails aborted!\nActiveRecord::ConnectionNotEstablished: no database\n" + strings.Repeat("/gems/rake/task.rb:241:in 'each'\n", 400) + "Tasks: TOP => db:prepare\n"
	got := Excerpt(out, 3000)
	if len(got) > 3010 || !strings.Contains(got, "ConnectionNotEstablished: no database") || !strings.HasSuffix(got, "Tasks: TOP => db:prepare\n") {
		t.Fatalf("excerpt = %q", got)
	}
}

func TestSetupFault(t *testing.T) {
	for _, c := range []struct{ out, kind string }{
		{"/gems/pg/connection.rb:832:in 'polling_loop': connection to server at \"::1\", port 5432 failed: Connection refused (PG::ConnectionBad)", "service"},
		{"Error: connect ECONNREFUSED 127.0.0.1:6379", "service"},
		{"LoadError: Could not open library 'vips.so.42': vips.so.42: cannot open shared object file: No such file or directory", "library"},
		{"Errno::ENOENT: No such file or directory - ffmpeg", "program"},
		{"sh: 1: pg_dump: not found", "program"},
		{"Error: spawn ffprobe ENOENT", "program"},
		{`internal/store/store.go:20:2: example.com/db@v1.2.3: Get "https://storage.example.com/mod.zip?sig=x": Forbidden`, "network"},
		{"curl: (56) CONNECT tunnel failed, response 403", "network"},
		// The code's own failures, not the sandbox's.
		{"Faraday::ConnectionFailed: Connection refused - connect(2) for \"api.example.com\" port 443", ""},
		{"Errno::ENOENT: No such file or directory - config/settings.yml", ""},
		{"Expected 2, got 3", ""},
		{`Error: Get "https://api.example.com/v1": unexpected status 403 Forbidden`, ""},
	} {
		f := SetupFault("noise\n" + c.out + "\nmore noise")
		if got := ""; f != nil {
			got = f.Kind
			if got != c.kind {
				t.Errorf("%q: kind %q, want %q", c.out, got, c.kind)
			}
		} else if c.kind != "" {
			t.Errorf("%q: no fault, want %q", c.out, c.kind)
		}
	}
}

func TestNetworkFaultNamesTheBlockedHost(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{`example.com/db@v1.2.3: Get "https://storage.example.com/mod.zip?sig=x": Forbidden`, "Add storage.example.com to `runner.egress.allow`"},
		{"factory egress proxy: files.example.com:443: not on the allowlist. To let jobs reach it, add it to runner.egress.allow in factory.yaml.", "Add files.example.com to"},
		{"curl: (56) CONNECT tunnel failed, response 403", "Add the host it names to"},
	} {
		f := SetupFault(c.line)
		if f == nil || f.Kind != "network" {
			t.Fatalf("%q: fault %+v, want network", c.line, f)
		}
		if got := f.Fix("o/r"); !strings.HasPrefix(got, c.want) {
			t.Errorf("%q: fix %q, want it to start %q", c.line, got, c.want)
		}
	}
}
