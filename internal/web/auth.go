package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Login guards the control room with a password once it's set: a signed
// session cookie after sign-in, a 401 for the API and a redirect to the
// sign-in page for everything else. The cookie's key is derived from a
// secret kept in the data directory and the password, so changing the
// password signs everyone out.
type Login struct {
	Password string
	Secret   []byte        // from LoadSecret
	TTL      time.Duration // default 30 days

	mu       sync.Mutex
	failures map[string][]time.Time // per client address, the last minute's
}

const sessionCookie = "factory_session"

// maxFailures is how many wrong passwords an address may try a minute.
const maxFailures = 5

// LoadSecret reads the cookie secret at path, making one the first time.
func LoadSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if k, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(k) >= 32 {
			return k, nil
		}
		return nil, fmt.Errorf("%s is not a 32-byte hex secret; delete it to make a new one", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func (l *Login) key() []byte {
	m := hmac.New(sha256.New, l.Secret)
	m.Write([]byte("factory session\x00" + l.Password))
	return m.Sum(nil)
}

func (l *Login) ttl() time.Duration {
	if l.TTL > 0 {
		return l.TTL
	}
	return 30 * 24 * time.Hour
}

// token is a session that expires at exp: the time, then its signature.
func (l *Login) token(exp time.Time) string {
	ts := strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, l.key())
	m.Write([]byte(ts))
	return ts + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (l *Login) valid(tok string, now time.Time) bool {
	ts, _, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Unix() >= n {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(l.token(time.Unix(n, 0))))
}

func (l *Login) signedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && l.valid(c.Value, time.Now())
}

// limited records a wrong password from r's address and reports whether
// it has had too many in the last minute.
func (l *Login) limited(r *http.Request, failed bool) bool {
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	now := time.Now()
	var recent []time.Time
	for _, t := range l.failures[addr] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if failed {
		recent = append(recent, now)
	}
	l.failures[addr] = recent
	return len(recent) >= maxFailures
}

func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign in · Factory</title>
<style>
:root{color-scheme:light dark;--bg:#f6f6f4;--card:#fff;--text:#1c1c1a;--muted:#6b6b66;--line:#ddd;--accent:#2f5bd3}
@media (prefers-color-scheme:dark){:root{--bg:#141413;--card:#1e1e1c;--text:#ececea;--muted:#9a9a94;--line:#333;--accent:#7d9cf0}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--text);font:16px/1.4 system-ui,sans-serif}
form{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px;width:min(340px,calc(100vw - 32px));box-sizing:border-box}
h1{font-size:20px;margin:0 0 16px}label{display:block;font-size:14px;color:var(--muted);margin-bottom:6px}
input{width:100%;box-sizing:border-box;padding:10px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit;font:inherit}
button{margin-top:16px;width:100%;padding:10px;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;cursor:pointer}
.err{color:#c4372f;font-size:14px;margin:12px 0 0}
</style></head><body>
<form method="post" action="/login">
<h1>Factory</h1>
<label for="pw">Password</label>
<input id="pw" name="password" type="password" autocomplete="current-password" autofocus required>
<input type="hidden" name="next" value="{{.Next}}">
<button type="submit">Sign in</button>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
</form></body></html>`))

// safeNext keeps a post-sign-in redirect on this site.
func safeNext(s string) string {
	u, err := url.Parse(s)
	if err != nil || s == "" || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/\\") || u.Host != "" {
		return "/"
	}
	return s
}

// wrap puts the sign-in in front of next.
func (l *Login) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = loginPage.Execute(w, map[string]string{"Next": safeNext(r.URL.Query().Get("next"))})
			return
		case r.URL.Path == "/login" && r.Method == http.MethodPost:
			l.signIn(w, r)
			return
		case r.URL.Path == "/logout" && r.Method == http.MethodPost:
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure(r)})
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		case l.signedIn(r):
			next.ServeHTTP(w, r)
			return
		case strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get("HX-Request") != "" || r.Method != http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"sign in first"}`))
			return
		default:
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		}
	})
}

func (l *Login) signIn(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	if l.limited(r, false) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = loginPage.Execute(w, map[string]string{"Next": next, "Error": "Too many tries. Wait a minute and try again."})
		return
	}
	want := sha256.Sum256([]byte(l.Password))
	got := sha256.Sum256([]byte(r.FormValue("password")))
	if !hmac.Equal(want[:], got[:]) {
		l.limited(r, true)
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		_ = loginPage.Execute(w, map[string]string{"Next": next, "Error": "That isn't the password."})
		return
	}
	exp := time.Now().Add(l.ttl())
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: l.token(exp), Path: "/", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure(r)})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// NeedsLogin reports whether listening on addr exposes the control room
// beyond this machine and a tailnet: anything but loopback and Tailscale
// addresses (100.64.0.0/10, fd7a:115c:a1e0::/48). A host name counts as
// exposed.
func NeedsLogin(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "localhost" {
		return false
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return true
	}
	a = a.Unmap()
	tailnet := netip.MustParsePrefix("100.64.0.0/10").Contains(a) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(a)
	return !a.IsLoopback() && !tailnet
}
