package pipeline

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// snapshot is a hash of every file in an export, taken before any check
// runs. Checks run the commit's own code on a writable export, so each one
// is followed by a look at the snapshot: a check that edits or deletes the
// commit's files (a lint config, a test) could pass a change the diff
// guard never saw.
type snapshot map[string][32]byte

func takeSnapshot(dir string) (snapshot, error) {
	s := snapshot{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		h, err := hashFile(path, d)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		s[rel] = h
		return nil
	})
	return s, err
}

func hashFile(path string, d fs.DirEntry) ([32]byte, error) {
	if d.Type()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(path)
		return sha256.Sum256([]byte("link:" + t)), err
	}
	b, err := os.ReadFile(path)
	return sha256.Sum256(b), err
}

// changed lists the commit's files that are now different or gone.
func (s snapshot) changed(dir string) []string {
	var out []string
	for rel, want := range s {
		path := filepath.Join(dir, rel)
		fi, err := os.Lstat(path)
		if err != nil {
			out = append(out, rel+" (deleted)")
			continue
		}
		if got, err := hashFile(path, fs.FileInfoToDirEntry(fi)); err != nil || got != want {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	if len(out) > 10 {
		out = append(out[:10], "…")
	}
	return out
}

// tamperNote says why a check's result is void, or "" if the commit's
// files are as exported.
func (s snapshot) tamperNote(dir string) string {
	c := s.changed(dir)
	if len(c) == 0 {
		return ""
	}
	return "this check changed the commit's own files (" + strings.Join(c, ", ") + "), so its result can't be trusted; checks may only add files"
}
