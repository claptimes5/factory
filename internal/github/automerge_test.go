package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAutoMerge(t *testing.T) {
	for _, c := range []struct {
		clean  bool
		checks string
		want   string
		err    bool
	}{
		{false, "", "ready|auto SQUASH abc", false},
		{true, `[{"status":"completed","conclusion":"success"}]`, `ready|auto SQUASH abc|merge {"merge_method":"squash","sha":"abc"}`, false},
		// No CI at all: GitHub would merge it untested.
		{true, `[]`, "ready|auto SQUASH abc", true},
		{true, `[{"status":"in_progress"}]`, "ready|auto SQUASH abc", true},
	} {
		clean := c.clean
		var calls []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			switch {
			case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/7":
				w.Write([]byte(`{"node_id":"PR_x","draft":true}`))
			case r.URL.Path == "/graphql":
				var q struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				json.Unmarshal(b, &q)
				if strings.Contains(q.Query, "markPullRequestReadyForReview") {
					calls = append(calls, "ready")
					w.Write([]byte(`{"data":{}}`))
					return
				}
				calls = append(calls, "auto "+q.Variables["method"].(string)+" "+q.Variables["sha"].(string))
				if clean {
					w.Write([]byte(`{"errors":[{"message":"Pull request Pull request is in clean status"}]}`))
					return
				}
				w.Write([]byte(`{"data":{}}`))
			case r.URL.Path == "/repos/o/r/commits/abc/check-runs":
				w.Write([]byte(`{"check_runs":` + c.checks + `}`))
			case r.Method == "PUT" && r.URL.Path == "/repos/o/r/pulls/7/merge":
				calls = append(calls, "merge "+string(b))
				w.Write([]byte(`{"merged":true}`))
			default:
				w.WriteHeader(404)
			}
		}))
		cl := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
		err := cl.AutoMerge(context.Background(), 7, "abc", "squash")
		srv.Close()
		if got := strings.Join(calls, "|"); (err != nil) != c.err || got != c.want {
			t.Errorf("%+v: %v\ngot  %s", c, err, got)
		}
	}
}
