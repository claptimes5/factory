package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func rules(t *testing.T, ss ...string) []Rule {
	t.Helper()
	var out []Rule
	for _, s := range ss {
		r, err := ParseRule(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestParseRule(t *testing.T) {
	for _, bad := range []string{"", "localhost", "1.2.3.4", "*", "a b.com", "http://x.com", "user@x.com"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	r := rules(t, "API.Example.com.", "*.cdn.example.com", "git.corp.example:8443")
	for _, c := range []struct {
		host, port string
		want       bool
	}{
		{"api.example.com", "443", true}, {"api.example.com", "80", true}, {"api.example.com", "22", false},
		{"evil.api.example.com", "443", false}, {"x.cdn.example.com", "443", true}, {"cdn.example.com", "443", false},
		{"evilcdn.example.com", "443", false}, {"git.corp.example", "8443", true}, {"git.corp.example", "443", false},
	} {
		got := false
		for _, x := range r {
			got = got || x.match(c.host, c.port)
		}
		if got != c.want {
			t.Errorf("%s:%s = %v", c.host, c.port, got)
		}
	}
}

// newProxy is a proxy whose names resolve per resolve, reaching up.
func newProxy(t *testing.T, resolve map[string]string, rs []Rule) *httptest.Server {
	t.Helper()
	p := &Proxy{Rules: rs,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			a, ok := resolve[host]
			if !ok {
				return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			}
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		},
		// The test servers are on loopback; 127.0.0.2 stands for a
		// private address.
		Blocked: func(a netip.Addr) bool { return a.String() == "127.0.0.2" || a.String() == "169.254.169.254" },
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

func TestProxyForwardsOnlyAllowedHosts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello from "+r.Host) }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	px := newProxy(t, map[string]string{"api.example.com": "127.0.0.1", "inside.example.com": "127.0.0.2", "meta.example.com": "169.254.169.254"},
		rules(t, "api.example.com:"+port, "inside.example.com:"+port, "meta.example.com:"+port))
	pu, _ := url.Parse(px.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	get := func(u string) (int, string) {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("http://api.example.com:" + port + "/x"); code != 200 || !strings.Contains(body, "hello from api.example.com") {
		t.Fatalf("allowed: %d %s", code, body)
	}
	for _, u := range []string{"http://other.example.com:" + port, "http://127.0.0.1:" + port, "http://inside.example.com:" + port, "http://meta.example.com:" + port} {
		if code, body := get(u); code != 403 || !strings.Contains(body, "runner.egress.allow") {
			t.Errorf("%s: %d %s", u, code, body)
		}
	}
}

func TestProxyTunnels(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	defer up.Close()
	// The client asks for api.example.com:443; the test server stands in.
	p := &Proxy{Rules: rules(t, "api.example.com"),
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Dial: func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(up.URL, "https://"))
		},
		Blocked: func(netip.Addr) bool { return false },
	}
	px := httptest.NewServer(p)
	defer px.Close()
	port := "443"
	pu, _ := url.Parse(px.URL)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(pu)
	tr.TLSClientConfig.InsecureSkipVerify = true // the test certificate is for example.com
	resp, err := (&http.Client{Transport: tr}).Get("https://api.example.com:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "secure" {
		t.Fatalf("body = %q", b)
	}
	// A ClientHello naming another host than the CONNECT is cut off.
	tr2 := tr.Clone()
	tr2.TLSClientConfig.ServerName = "evil.example.com"
	if _, err := (&http.Client{Transport: tr2}).Get("https://api.example.com:" + port + "/"); err == nil {
		t.Fatal("a tunnel carried TLS for another name")
	}
	// CONNECT to a host off the list is refused before any tunnel.
	conn, err := net.Dial("tcp", strings.TrimPrefix(px.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "CONNECT evil.example.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || r.StatusCode != 403 {
		t.Fatalf("CONNECT evil: %v %v", r, err)
	}
}

func TestBlocked(t *testing.T) {
	for a, want := range map[string]bool{"127.0.0.1": true, "10.1.2.3": true, "192.168.1.1": true, "172.17.0.1": true,
		"169.254.169.254": true, "100.100.1.1": true, "::1": true, "fd00::1": true, "0.0.0.0": true, "::ffff:10.0.0.1": true,
		"140.82.112.3": false, "2606:4700::1": false, "64:ff9b::a00:1": true, "::10.0.0.1": true, "0.1.2.3": true, "198.18.0.1": true, "240.0.0.1": true} {
		if got := Blocked(netip.MustParseAddr(a)); got != want {
			t.Errorf("%s: %v", a, got)
		}
	}
	if !privateOnly(netip.MustParseAddr("10.0.0.5")) || privateOnly(netip.MustParseAddr("127.0.0.1")) || privateOnly(netip.MustParseAddr("169.254.169.254")) {
		t.Error("privateOnly")
	}
}

// A tunnel to 443 that sends no ClientHello in time is closed, not
// passed through unchecked.
func TestSilentTLSTunnelIsClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c)
		got <- b
	}()
	p := &Proxy{Rules: rules(t, "api.example.com"), HelloWait: 200 * time.Millisecond,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Dial: func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
		},
		Blocked: func(netip.Addr) bool { return false },
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	if r, err := http.ReadResponse(br, nil); err != nil || r.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", r, err)
	}
	time.Sleep(400 * time.Millisecond)
	conn.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 1, 2, 3, 4, 5})
	select {
	case b := <-got:
		if len(b) > 0 {
			t.Fatalf("upstream got %q after the wait", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the tunnel stayed open")
	}
}

// A long download the client sends nothing back on outlives the idle
// limit: only silence both ways closes a tunnel.
func TestIdleCountsBothWays(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for i := range 10 { // a second of trickle
			fmt.Fprintf(c, "chunk%d\n", i)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p := &Proxy{Rules: rules(t, "dl.example.com:"+port),
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Blocked: func(netip.Addr) bool { return false },
		Idle:    300 * time.Millisecond,
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT dl.example.com:%s HTTP/1.1\r\nHost: dl.example.com:%s\r\n\r\n", port, port)
	br := bufio.NewReader(conn)
	if r, err := http.ReadResponse(br, nil); err != nil || r.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", r, err)
	}
	b, _ := io.ReadAll(br)
	if !strings.Contains(string(b), "chunk9") {
		t.Fatalf("download cut short: %q", b)
	}
}
