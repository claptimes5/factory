package verdict

import (
	"strings"
	"testing"
)

func TestGroomParse(t *testing.T) {
	t.Parallel()
	var g Groom
	ok := `{"duplicates":[{"canonical":1,"duplicates":[2],"why":"Both ask for dark mode."}],"stale":[],"too_big":[],"summary":"one pair"}`
	if err := Parse([]byte(ok), &g); err != nil || len(g.Duplicates) != 1 {
		t.Fatalf("parse: %v %+v", err, g)
	}
	many := `{"duplicates":[],"stale":[` + strings.TrimSuffix(strings.Repeat(`{"issue":1,"why":"x"},`, MaxGroomEntries+1), ",") + `],"too_big":[],"summary":""}`
	for _, bad := range []string{
		`{"duplicates":[],"stale":[],"summary":""}`,                                     // too_big missing
		`{"duplicates":[],"stale":[],"too_big":[],"summary":"","extra":1}`,              // unknown field
		`{"duplicates":[],"stale":[{"issue":"7","why":"x"}],"too_big":[],"summary":""}`, // id isn't a number
		many,
	} {
		if err := Parse([]byte(bad), &Groom{}); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Schema("groom"); err != nil {
		t.Fatal(err)
	}
}

func TestGroomSift(t *testing.T) {
	t.Parallel()
	scope := GroomScope{
		Flaggable: map[int]bool{2: true, 3: true, 4: true, 5: true},
		Old:       map[int]bool{2: true, 3: true},
		Known:     map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 9: true},
	}
	split := func(n int) []GroomChild {
		var out []GroomChild
		for i := 0; i < n; i++ {
			out = append(out, GroomChild{Title: "Part", Body: "Do a part of #4."})
		}
		return out
	}
	g := Groom{
		Duplicates: []GroomDuplicate{
			{Canonical: 9, Duplicates: []int{2, 9, 77}, Why: "Same as #9: both want CSV export."}, // 9 is busy but may be canonical
			{Canonical: 66, Duplicates: []int{5}, Why: "x"},
			{Canonical: 1, Duplicates: []int{5}, Why: "See https://evil.example"},
		},
		Stale: []GroomStale{
			{Issue: 3, Why: "The old API it names is gone."},
			{Issue: 4, Why: "Recent, so not stale."},
			{Issue: 2, Why: "Already flagged as a duplicate."},
		},
		TooBig: []GroomTooBig{
			{Issue: 4, Why: "Three features in one.", Split: split(3)},
			{Issue: 5, Why: "Too big.", Split: split(6)},
			{Issue: 5, Why: "Too big.", Split: []GroomChild{{Title: "A", Body: "ping @alice"}, {Title: "B", Body: "b"}}},
		},
	}
	kept, dropped := g.Sift(scope)
	if len(kept.Duplicates) != 1 || len(kept.Duplicates[0].Duplicates) != 1 || kept.Duplicates[0].Duplicates[0] != 2 {
		t.Fatalf("duplicates %+v", kept.Duplicates)
	}
	if len(kept.Stale) != 1 || kept.Stale[0].Issue != 3 {
		t.Fatalf("stale %+v", kept.Stale)
	}
	if len(kept.TooBig) != 1 || kept.TooBig[0].Issue != 4 || len(kept.TooBig[0].Split) != 3 {
		t.Fatalf("too big %+v", kept.TooBig)
	}
	all := strings.Join(dropped, "\n")
	for _, want := range []string{"#9 is its own canonical", "#77 wasn't in the input", "canonical #66", "has a link",
		"#4 had activity", "#2 is already flagged", "into 6", "has an @mention"} {
		if !strings.Contains(all, want) {
			t.Errorf("dropped doesn't say %q:\n%s", want, all)
		}
	}
	for text, want := range map[string]string{
		"plain words":            "",
		"see [here](x)":          "has a link",
		"see www.example.com":    "has a link",
		"cc @team":               "has an @mention",
		"<img src=x>":            "has HTML",
		"fixed by !12":           "reference",
		"see GH-123":             "reference",
		"see o/other#4":          "reference",
		"part of &3":             "reference",
		"in *iteration:5":        "reference",
		"[vulnerability:1]":      "reference",
		"label ~bug":             "reference",
		"costs $5, saves 50%.":   "",
		"about ~5 files":         "",
		"Steps\n/close":          "quick action",
		"see #123":               "#123",
		"see #1 and #9":          "",
		strings.Repeat("a", 601): "over 600",
		"   ":                    "is empty",
		"javascript:alert(1)":    "has a link",
		"x\u202etxt.exe":         "control",
		"a\u200bhidden space":    "control",
		"bell\a":                 "control",
		"escape \x1b[31m":        "control",
		"tab\tand\nnewline":      "",
	} {
		err := scope.text(text, MaxGroomWhy, false)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
	if err := scope.text("two\nlines", MaxChildTitle, true); err == nil {
		t.Error("a title may not break lines")
	}
}

// Of two issues, only one is ever proposed closed: a canonical issue is
// never flagged itself, and an issue already proposed closed is no one's
// canonical.
func TestGroomSiftCanonicals(t *testing.T) {
	t.Parallel()
	all := map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}
	scope := GroomScope{Flaggable: all, Old: all, Known: all, NotCanonical: map[int]bool{5: true}}
	g := Groom{
		Duplicates: []GroomDuplicate{
			{Canonical: 2, Duplicates: []int{1}, Why: "Same."},
			{Canonical: 1, Duplicates: []int{2}, Why: "Same, the other way."},
			{Canonical: 3, Duplicates: []int{2}, Why: "Canonical is flagged as a duplicate? no: 2 is canonical."},
			{Canonical: 5, Duplicates: []int{4}, Why: "5 is proposed closed already."},
		},
		Stale:  []GroomStale{{Issue: 2, Why: "The canonical one, stale."}},
		TooBig: []GroomTooBig{{Issue: 2, Why: "Big.", Split: []GroomChild{{Title: "a", Body: "costs $5"}, {Title: "b", Body: "c"}}}},
	}
	kept, dropped := g.Sift(scope)
	if len(kept.Duplicates) != 1 || kept.Duplicates[0].Canonical != 2 || len(kept.Stale)+len(kept.TooBig) != 0 {
		t.Fatalf("kept %+v\ndropped %q", kept, dropped)
	}
	joined := strings.Join(dropped, "\n")
	for _, want := range []string{"canonical #1 is already flagged", "#2 is a duplicate's canonical issue", "canonical #5 is itself proposed closed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dropped lacks %q:\n%s", want, joined)
		}
	}
	// Amounts are posted in a code span, so GitLab reads no reference.
	if got, err := scope.posted("costs $5 or ~5.5 days, `$7` kept", 100, false); err != nil || got != "costs `$5` or `~5.5` days, `$7` kept" {
		t.Fatalf("posted %q %v", got, err)
	}
}
