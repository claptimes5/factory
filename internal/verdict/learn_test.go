package verdict

import "testing"

func TestLearnParse(t *testing.T) {
	var l Learn
	if err := Parse([]byte(`{"lessons":[{"line":"Check for overflow in arithmetic helpers.","why":"#1 and #2","evidence":["#1","#2"]}],"skip_reason":""}`), &l); err != nil || len(l.Lessons) != 1 {
		t.Fatalf("parse: %v %+v", err, l)
	}
	for _, bad := range []string{
		`{"lessons":[],"skip_reason":""}`, // none needs a reason
		`{"lessons":[{"line":"","why":"x","evidence":[]}],"skip_reason":""}`,
		`{"lessons":[{"line":"a","why":"x","evidence":[]},{"line":"b","why":"x","evidence":[]},{"line":"c","why":"x","evidence":[]},{"line":"d","why":"x","evidence":[]}],"skip_reason":""}`,
		`{"skip_reason":"nothing"}`,
	} {
		if err := Parse([]byte(bad), &Learn{}); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := Parse([]byte(`{"lessons":[],"skip_reason":"one-off fixes"}`), &Learn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Schema("learn"); err != nil {
		t.Fatal(err)
	}
}
