// Package diffrisk rates a change's risk from its diff, once it's built:
// how big it is, what it touches, and the kinds of change that always
// need a person (a migration, a new dependency, auth code, a deleted
// test). Triage rates risk from the issue's text before any code exists;
// this rates what was actually written. It only reads: the caller keeps
// the higher of the two ratings.
//
// Everything the diff names (a path, a package, a test) is the builder's
// to choose, so a Reason keeps it apart from the factory's own words, in
// Items, and Text quotes each one with the caller's quoting.
package diffrisk

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/pathglob"
)

// The kinds of reason a diff raises risk for. The last four are the ones
// risk.needs_person can name.
const (
	KindLines       = "lines"
	KindFiles       = "files"
	KindPaths       = "paths"
	KindFeature     = "feature"
	KindUnread      = "unread"
	Migrations      = "migrations"
	NewDependencies = "new_dependencies"
	Auth            = "auth"
	DeletedTests    = "deleted_tests"
)

// Kinds are the change kinds risk.needs_person may list.
var Kinds = []string{Migrations, NewDependencies, Auth, DeletedTests}

// Rules are risk's settings, from factory.yaml.
type Rules struct {
	// A change of at least this many changed lines, or files, is at
	// least medium or high risk; 0 turns a threshold off. Lockfiles
	// don't count toward the lines.
	MediumLines, HighLines int
	MediumFiles, HighFiles int
	// A change touching any of these paths is at least medium or high.
	MediumPaths, HighPaths []string
	// Auth are the areas of code a change to which counts as auth (auth,
	// crypto, secrets, payments), and Migrations a database's migrations.
	Auth, Migrations []pathglob.Area
	// NeedsPerson are the kinds that always wait for a person.
	NeedsPerson []string
}

// File is one file a change touches.
type File struct {
	Path string
	// Status is git's: A added, D deleted, M modified, T type changed.
	Status         string
	Added, Deleted int
	Binary         bool
}

// Change is what Rate reads: the files, the patch, and each file's
// contents before and after (ok false where it isn't there).
type Change struct {
	Files []File
	Patch string
	// Before and After read a file at the base and at the head.
	Before, After func(path string) (body string, ok bool, err error)
}

// Item is something the diff names, as the diff has it: untrusted.
type Item struct {
	Name  string `json:"name,omitempty"`  // a package, a test, a feature or a glob
	File  string `json:"file,omitempty"`  // the file it's in
	Count int    `json:"count,omitempty"` // how many, when more than one
}

// Reason is one thing about the diff that raised its risk.
type Reason struct {
	Kind string `json:"kind"`
	Risk string `json:"risk"`
	// Label is the workspace repo's folder it was found in; "" alone.
	Label string `json:"label,omitempty"`
	// What is what the diff does, in the factory's words, as a phrase
	// that follows "it": "adds or changes a migration".
	What string `json:"what"`
	// Items are what it names; More counts those left out.
	Items []Item `json:"items,omitempty"`
	More  int    `json:"more,omitempty"`
	// NeedsPerson: risk.needs_person lists its kind (or the diff couldn't
	// be rated), so the change waits for a person whatever its risk.
	NeedsPerson bool `json:"needs_person,omitempty"`
}

// maxItems is how many items a reason keeps.
const maxItems = 10

// Text is the reason as a phrase that follows "it", led by its label,
// with each item through quote (which must make it inert: a code span).
func (r Reason) Text(quote func(string) string) string {
	var b strings.Builder
	if r.Label != "" {
		b.WriteString(quote(r.Label) + ": ")
	}
	b.WriteString(r.What)
	if len(r.Items) == 0 {
		return b.String()
	}
	var parts []string
	for _, it := range r.Items {
		var s string
		switch {
		case it.Name != "" && it.File != "":
			s = quote(it.Name) + " in " + quote(it.File)
		case it.Name != "":
			s = quote(it.Name)
		default:
			s = quote(it.File)
		}
		if it.Count > 1 {
			s = fmt.Sprintf("%d × %s", it.Count, s)
		}
		parts = append(parts, s)
	}
	if r.More > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", r.More))
	}
	b.WriteString(": " + strings.Join(parts, ", "))
	return b.String()
}

// Code is a quote for Text where nothing renders markdown, as a plain
// code span: one short line with no backtick to close it.
func Code(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case unicode.IsControl(r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return "`" + s + "`"
}

// Assessment is the diff's rating. It is saved with the build job.
type Assessment struct {
	// Risk is the highest reason's risk; low with none.
	Risk    string   `json:"risk"`
	Reasons []Reason `json:"reasons"`
	// Unread are why the diff couldn't be rated in full. The change then
	// waits for a person, as its risk may be understated.
	Unread []Reason `json:"unread,omitempty"`
	// Raised says the rating raised the item's risk above triage's (and
	// the feature map's).
	Raised bool `json:"raised,omitempty"`
}

// NeedsPerson are the reasons the change waits for a person.
func (a Assessment) NeedsPerson() []Reason {
	var out []Reason
	for _, r := range a.Reasons {
		if r.NeedsPerson {
			out = append(out, r)
		}
	}
	return append(out, a.Unread...)
}

// Add records a reason, and raises the rating to its risk.
func (a *Assessment) Add(r Reason, rules Rules) {
	r.NeedsPerson = slices.Contains(rules.NeedsPerson, r.Kind)
	a.Reasons = append(a.Reasons, r)
	if Higher(r.Risk, a.Risk) {
		a.Risk = r.Risk
	}
}

// Unreadable records what couldn't be read, in the factory's words ("couldn't
// list the changed files"), and the file when there's one.
func (a *Assessment) Unreadable(what, file string) {
	r := Reason{Kind: KindUnread, What: "couldn't be rated from its diff: the factory " + what, NeedsPerson: true}
	if file != "" {
		r.Items = []Item{{File: file}}
	}
	a.Unread = append(a.Unread, r)
}

// Merge adds b's reasons to a, each labeled with label (a workspace
// repo's folder) when it's set.
func (a *Assessment) Merge(b Assessment, label string) {
	for _, r := range b.Reasons {
		r.Label = label
		a.Reasons = append(a.Reasons, r)
		if Higher(r.Risk, a.Risk) {
			a.Risk = r.Risk
		}
	}
	for _, r := range b.Unread {
		r.Label = label
		a.Unread = append(a.Unread, r)
	}
}

var rank = map[string]int{"": 0, "low": 1, "medium": 2, "high": 3}

// Higher reports whether risk a is above b.
func Higher(a, b string) bool { return rank[a] > rank[b] }

// binaryLines is what one changed binary file counts toward the lines,
// as the diff guard counts it.
const binaryLines = 100

// Rate rates one repo's change. A file it can't read leaves the rating
// unread, with what it did find.
func Rate(ch Change, r Rules) Assessment {
	a := Assessment{Risk: "low"}
	lines := 0
	for _, f := range ch.Files {
		switch {
		case isLockfile(f.Path):
		case f.Binary:
			lines += binaryLines
		default:
			lines += f.Added + f.Deleted
		}
	}
	switch {
	case r.HighLines > 0 && lines >= r.HighLines:
		a.Add(Reason{Kind: KindLines, Risk: "high", What: fmt.Sprintf("changes %d lines (risk.high_lines is %d)", lines, r.HighLines)}, r)
	case r.MediumLines > 0 && lines >= r.MediumLines:
		a.Add(Reason{Kind: KindLines, Risk: "medium", What: fmt.Sprintf("changes %d lines (risk.medium_lines is %d)", lines, r.MediumLines)}, r)
	}
	switch n := len(ch.Files); {
	case r.HighFiles > 0 && n >= r.HighFiles:
		a.Add(Reason{Kind: KindFiles, Risk: "high", What: fmt.Sprintf("changes %d files (risk.high_files is %d)", n, r.HighFiles)}, r)
	case r.MediumFiles > 0 && n >= r.MediumFiles:
		a.Add(Reason{Kind: KindFiles, Risk: "medium", What: fmt.Sprintf("changes %d files (risk.medium_files is %d)", n, r.MediumFiles)}, r)
	}
	var high, medium, migrations, deleted []string
	auth := map[string][]Item{} // by area
	for _, f := range ch.Files {
		test := gates.IsTest(f.Path)
		switch {
		case pathglob.MatchAny(r.HighPaths, f.Path):
			high = append(high, f.Path)
		case pathglob.MatchAny(r.MediumPaths, f.Path):
			medium = append(medium, f.Path)
		}
		code := !test && !notCode(f.Path)
		if area, g, ok := pathglob.InAreas(r.Auth, f.Path); ok && code && !vendored(f.Path) {
			auth[area] = append(auth[area], Item{Name: g, File: f.Path})
		}
		if _, _, ok := pathglob.InAreas(r.Migrations, f.Path); ok && code {
			migrations = append(migrations, f.Path)
		}
		if test && f.Status == "D" {
			deleted = append(deleted, f.Path)
		}
	}
	if len(high) > 0 {
		a.Add(files(KindPaths, "high", "touches a path in risk.high_paths", high), r)
	}
	if len(medium) > 0 {
		a.Add(files(KindPaths, "medium", "touches a path in risk.medium_paths", medium), r)
	}
	if len(migrations) > 0 {
		a.Add(files(Migrations, "high", "adds or changes a migration", migrations), r)
	}
	for _, ar := range r.Auth {
		if its := auth[ar.Name]; len(its) > 0 {
			sort.Slice(its, func(i, j int) bool { return its[i].File < its[j].File })
			a.Add(items(Auth, "high", "changes "+ar.Name+" code", its), r)
			delete(auth, ar.Name) // an area may be listed twice
		}
	}
	deps, unknown := newDependencies(ch, &a)
	if len(deps) > 0 {
		a.Add(items(NewDependencies, "medium", "adds a dependency, or changes where one comes from", deps), r)
	}
	if len(unknown) > 0 {
		a.Add(files(NewDependencies, "medium", "changes a dependency manifest the factory doesn't read, which counts as a new dependency", unknown), r)
	}
	tests := removedTests(ch.Patch)
	for _, f := range deleted {
		tests = append(tests, Item{File: f})
	}
	if len(tests) > 0 {
		a.Add(items(DeletedTests, "medium", "deletes tests", tests), r)
	}
	return a
}

// files is a reason naming files.
func files(kind, risk, what string, fs []string) Reason {
	sort.Strings(fs)
	var its []Item
	for _, f := range fs {
		its = append(its, Item{File: f})
	}
	return items(kind, risk, what, its)
}

// items is a reason naming items, the first maxItems of them.
func items(kind, risk, what string, its []Item) Reason {
	r := Reason{Kind: kind, Risk: risk, What: what, Items: its}
	if len(its) > maxItems {
		r.Items, r.More = its[:maxItems], len(its)-maxItems
	}
	return r
}

// docExts are files that are never code, whatever their name says.
var docExts = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".adoc": true, ".txt": true, ".png": true, ".jpg": true,
	".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".webp": true, ".pdf": true, ".snap": true}

func notCode(p string) bool { return docExts[strings.ToLower(path.Ext(p))] }

// vendored is code someone else wrote, checked in.
func vendored(p string) bool {
	p = "/" + strings.ToLower(p)
	return strings.Contains(p, "/vendor/") || strings.Contains(p, "/node_modules/") || strings.Contains(p, "/third_party/")
}
