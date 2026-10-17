package verdict

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewParse(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		ok     bool
		blocks bool
	}{
		{"pass", `{"decision":"pass","findings":[],"challenged_assumptions":[]}`, true, false},
		{"block", `{"decision":"block","findings":[{"severity":"major","file":"a.go","line":3,"claim":"x","evidence":"y"}],"challenged_assumptions":[]}`, true, true},
		{"critical overrides pass", `{"decision":"pass","findings":[{"severity":"critical","file":"a.go","line":1,"claim":"x","evidence":"y"}],"challenged_assumptions":[]}`, true, true},
		{"fenced", "Here you go:\n```json\n{\"decision\":\"pass\",\"findings\":[],\"challenged_assumptions\":[]}\n```", true, false},
		{"bad decision", `{"decision":"lgtm","findings":[],"challenged_assumptions":[]}`, false, false},
		{"missing findings", `{"decision":"pass"}`, false, false},
		{"unknown field", `{"decision":"pass","findings":[],"challenged_assumptions":[],"approve":true}`, false, false},
		{"empty", ``, false, false},
		{"prose", `looks good to me`, false, false},
		{"bad severity", `{"decision":"block","findings":[{"severity":"high","file":"","line":0,"claim":"x","evidence":""}],"challenged_assumptions":[]}`, false, false},
	}
	for _, tc := range cases {
		var r Review
		err := Parse([]byte(tc.raw), &r)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
			continue
		}
		if tc.ok && r.Blocks() != tc.blocks {
			t.Errorf("%s: Blocks()=%v", tc.name, r.Blocks())
		}
	}
}

func TestTriageParkNeedsQuestion(t *testing.T) {
	var tr Triage
	if err := Parse([]byte(`{"decision":"park","complexity":"S","risk":"low","question":"","summary":""}`), &tr); err == nil {
		t.Fatal("park without question must fail")
	}
	if err := Parse([]byte(`{"decision":"ready","complexity":"M","risk":"low","question":"","summary":"ok"}`), &tr); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPrefersFileThenFinal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "verdict.json")
	var r Review
	if _, err := Load(p, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`, &r); err != nil {
		t.Fatalf("fallback to final message: %v", err)
	}
	os.WriteFile(p, []byte(`{"decision":"block","findings":[],"challenged_assumptions":[]}`), 0o644)
	if _, err := Load(p, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`, &r); err != nil || r.Decision != "block" {
		t.Fatalf("file should win: %v %s", err, r.Decision)
	}
	if _, err := Load(filepath.Join(dir, "nope.json"), "", &r); err == nil {
		t.Fatal("no file and no final message must fail")
	}
}

func TestSchemasAreJSON(t *testing.T) {
	for k, s := range schemas {
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Errorf("%s schema: %v", k, err)
		}
	}
}
