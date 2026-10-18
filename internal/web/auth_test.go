package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLogin(t *testing.T) {
	t.Parallel()
	l := &Login{Password: "hunter22", Secret: make([]byte, 32)}
	h := l.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("inside")) }))
	do := func(method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		r := httptest.NewRequest(method, path, body)
		r.RemoteAddr = "10.0.0.9:1234"
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/board?x=1", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fboard%3Fx%3D1" {
		t.Fatalf("page: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do("GET", "/api/v1/overview", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("api: %d", w.Code)
	}
	if w := do("POST", "/jobs/1/retry", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("action: %d", w.Code)
	}
	if w := do("POST", "/login", url.Values{"password": {"wrong"}}, nil); w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w := do("POST", "/login", url.Values{"password": {"hunter22"}, "next": {"//evil.example/x"}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("sign in: %d %s", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %+v", c)
	}
	if w := do("GET", "/board", nil, c); w.Body.String() != "inside" {
		t.Fatalf("signed in: %d %s", w.Code, w.Body.String())
	}
	// A forged or expired session, or one from before a password change.
	for _, bad := range []string{"9999999999.AAAA", l.token(time.Now().Add(-time.Minute)), c.Value + "x"} {
		if w := do("GET", "/board", nil, &http.Cookie{Name: sessionCookie, Value: bad}); w.Code != http.StatusSeeOther {
			t.Errorf("session %q let in", bad)
		}
	}
	l2 := &Login{Password: "changed", Secret: l.Secret}
	if l2.valid(c.Value, time.Now()) {
		t.Error("a session outlived a password change")
	}
	// Five wrong tries a minute, then even the right one waits.
	for range maxFailures {
		do("POST", "/login", url.Values{"password": {"nope"}}, nil)
	}
	if w := do("POST", "/login", url.Values{"password": {"hunter22"}}, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after many failures: %d", w.Code)
	}
}

func TestNeedsLogin(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{"127.0.0.1:8080": false, "localhost:8080": false, "[::1]:8080": false,
		"100.101.102.103:8080": false, "0.0.0.0:8080": true, ":8080": true, "192.168.1.5:8080": true, "myhost:8080": true} {
		if got := NeedsLogin(addr); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func TestLoginLimits(t *testing.T) {
	t.Parallel()
	if clientKey("[2001:db8:1:2:3:4:5:6]:99") != clientKey("[2001:db8:1:2:ffff::1]:1") {
		t.Error("one IPv6 /64 should share a limit")
	}
	if clientKey("10.0.0.1:1") == clientKey("10.0.0.2:1") {
		t.Error("IPv4 addresses should have their own limits")
	}
	l := &Login{Password: "correct horse battery", Secret: make([]byte, 32)}
	req := func(addr string) *http.Request {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = addr
		return r
	}
	// Many addresses, a few guesses each, still hit the overall cap.
	limited := false
	for i := 0; i < maxAllFailures; i++ {
		limited = l.limited(req(fmt.Sprintf("10.0.%d.%d:1", i/250, i%250)), true)
	}
	if !limited || !l.limited(req("192.0.2.1:1"), false) {
		t.Error("the overall cap didn't hold")
	}
}
