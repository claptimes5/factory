package runner

import "testing"

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
