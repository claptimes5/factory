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
	for _, clean := range []bool{false, true} {
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
			case r.Method == "PUT" && r.URL.Path == "/repos/o/r/pulls/7/merge":
				calls = append(calls, "merge "+string(b))
				w.Write([]byte(`{"merged":true}`))
			default:
				w.WriteHeader(404)
			}
		}))
		c := &Client{Repo: "o/r", BaseURL: srv.URL, Tokens: StaticToken("tok")}
		err := c.AutoMerge(context.Background(), 7, "abc", "squash")
		srv.Close()
		want := "ready|auto SQUASH abc"
		if clean {
			want += `|merge {"merge_method":"squash","sha":"abc"}`
		}
		if got := strings.Join(calls, "|"); err != nil || got != want {
			t.Errorf("clean=%v: %v\ngot  %s\nwant %s", clean, err, got, want)
		}
	}
}
