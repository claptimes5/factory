// Package pathglob matches changed files against the globs that name
// sensitive code: the shared sensitive list, the security pass's paths and
// risk's. It is one matcher for all of them, so a glob means the same
// wherever it is set.
package pathglob

import (
	"errors"
	"path"
	"slices"
	"strings"
)

// Area is a named list of globs, such as crypto's.
type Area struct {
	Name  string
	Globs []string
}

// glob is a parsed pattern.
type glob struct {
	parts []string
	// rooted is matched from the repo's root, part by part; otherwise
	// the one part is matched against any part of the path.
	rooted bool
	// dir names a directory: it matches only files under one.
	dir bool
}

// parse reads a pattern, lowercased: see Match.
func parse(p string) (glob, error) {
	var g glob
	p = strings.ToLower(strings.TrimSpace(p))
	if strings.Contains(p, `\`) {
		return g, errors.New(`paths use /, not \`)
	}
	for {
		switch {
		case strings.HasPrefix(p, "./"):
			p = p[2:]
			continue
		case strings.HasPrefix(p, "/"):
			p = p[1:]
			continue
		}
		break
	}
	g.dir = strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return g, errors.New("is empty")
	}
	g.rooted = strings.Contains(p, "/") || strings.Contains(p, "**")
	for _, s := range strings.Split(p, "/") {
		switch {
		case s == "" || s == "." || s == "..":
			return g, errors.New("has an empty, . or .. part")
		case strings.Contains(s, "**") && s != "**":
			return g, errors.New("** must be a whole part of the path, as in a/**/b")
		}
		if _, err := path.Match(s, "x"); err != nil {
			return g, errors.New("is not a valid glob")
		}
		g.parts = append(g.parts, s)
	}
	return g, nil
}

// Valid says what's wrong with a pattern, or nil.
func Valid(p string) error {
	_, err := parse(p)
	return err
}

// Match matches a changed file against a pattern, ignoring case. A
// pattern with no / (`*auth*`, `go.mod`) matches any one part of the
// path, a directory's name or the file's, and ending in / only a
// directory's (`migrations/`). Any other is matched from the repo's
// root, part by part, where * stops at / and a part that is only **
// matches any number of parts. It matches what it names and everything
// under it, so `internal/billing` and `app/*/keys` match the files in
// those directories too; ending in /, only those (`db/migrate/`). A
// leading / or ./ changes nothing. A pattern Valid refuses matches
// nothing.
func Match(pattern, file string) bool {
	g, err := parse(pattern)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.ToLower(path.Clean("/" + file))[1:], "/")
	if g.rooted {
		return matchParts(g.parts, parts, g.dir)
	}
	if g.dir {
		parts = parts[:len(parts)-1]
	}
	return slices.ContainsFunc(parts, func(p string) bool {
		ok, _ := path.Match(g.parts[0], p)
		return ok
	})
}

// matchParts matches a rooted glob's parts against a path's. A path
// longer than the glob matches when the glob names a directory it is in;
// one as long, unless the glob is only for directories.
func matchParts(g, f []string, dir bool) bool {
	if len(g) == 0 {
		return !dir || len(f) > 0
	}
	if g[0] == "**" {
		for k := 0; k <= len(f); k++ {
			if matchParts(g[1:], f[k:], dir) {
				return true
			}
		}
		return false
	}
	if len(f) == 0 {
		return false
	}
	ok, _ := path.Match(g[0], f[0])
	return ok && matchParts(g[1:], f[1:], dir)
}

// First is the first of patterns that file matches.
func First(patterns []string, file string) (string, bool) {
	for _, p := range patterns {
		if Match(p, file) {
			return p, true
		}
	}
	return "", false
}

// MatchAny reports whether file matches one of patterns.
func MatchAny(patterns []string, file string) bool {
	_, ok := First(patterns, file)
	return ok
}

// InAreas is the first area, and its glob, that file matches.
func InAreas(areas []Area, file string) (area, glob string, ok bool) {
	for _, a := range areas {
		if g, ok := First(a.Globs, file); ok {
			return a.Name, g, true
		}
	}
	return "", "", false
}
