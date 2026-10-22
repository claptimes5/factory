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
		// The code's own failures, not the sandbox's.
		{"Faraday::ConnectionFailed: Connection refused - connect(2) for \"api.example.com\" port 443", ""},
		{"Errno::ENOENT: No such file or directory - config/settings.yml", ""},
		{"Expected 2, got 3", ""},
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
