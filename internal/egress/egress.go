// Package egress is the proxy every job's traffic leaves through. Jobs sit
// on a Docker network with no way out; the proxy, on that network and on
// one that does, forwards only to the hosts on its allowlist: the model
// APIs, the git host and package registries. A builder that reads a
// prompt injection can send what it holds only there.
package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Defaults are the hosts a job needs whatever the repo: the agents' APIs
// and sign-in, GitHub and GitLab, and the common package registries.
var Defaults = []string{
	// Claude
	"api.anthropic.com", "claude.ai", "console.anthropic.com", "platform.claude.com",
	// Codex
	"api.openai.com", "auth.openai.com", "chatgpt.com",
	// Git hosts
	"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com", "ghcr.io", "gitlab.com", "registry.gitlab.com",
	// Go
	"proxy.golang.org", "sum.golang.org",
	// JavaScript
	"registry.npmjs.org", "registry.yarnpkg.com", "repo.yarnpkg.com",
	// Python
	"pypi.org", "files.pythonhosted.org",
	// Ruby
	"rubygems.org", "index.rubygems.org",
	// Rust
	"crates.io", "index.crates.io", "static.crates.io", "static.rust-lang.org",
	// Java
	"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "services.gradle.org",
}

// Rule is one allowlist entry: a host, or "*.domain" for its subdomains,
// with an optional port (default 443 and 80).
type Rule struct {
	Host     string // lower case, no trailing dot; for a wildcard the domain
	Wildcard bool
	Port     string // "" for 443 and 80
	// Private lets the host be on a private network (a self-hosted git
	// host); loopback and link-local addresses stay out of reach.
	Private bool
}

// ParseRule reads "host", "*.domain" or either with ":port".
func ParseRule(s string) (Rule, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	r := Rule{}
	if h, p, err := net.SplitHostPort(s); err == nil {
		s, r.Port = h, p
	}
	if r.Wildcard = strings.HasPrefix(s, "*."); r.Wildcard {
		s = s[2:]
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" || strings.ContainsAny(s, "*/ @") || !strings.Contains(s, ".") {
		return Rule{}, fmt.Errorf("egress: %q is not a host name or *.domain", s)
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return Rule{}, fmt.Errorf("egress: %q is an address; allow a host name", s)
	}
	r.Host = s
	return r, nil
}

func (r Rule) match(host, port string) bool {
	if r.Port == "" && port != "443" && port != "80" || r.Port != "" && port != r.Port {
		return false
	}
	if r.Wildcard {
		return strings.HasSuffix(host, "."+r.Host)
	}
	return host == r.Host
}

// Proxy forwards HTTP and CONNECT requests to allowlisted hosts only.
type Proxy struct {
	Rules []Rule
	Log   *slog.Logger
	// Resolve and Dial are net's, replaced in tests.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, addr string) (net.Conn, error)
	// Blocked says an address may never be reached: the host's, the
	// cloud metadata service, private networks. Replaced in tests.
	Blocked func(netip.Addr) bool

	once  sync.Once
	tr    *http.Transport
	slots chan struct{}
}

// blockedNets are ranges no allowlisted name should resolve into: shared
// address space (carrier-grade NAT, Tailscale), "this network", benchmark,
// reserved, NAT64 and IPv4-compatible IPv6 (which reach IPv4 addresses).
var blockedNets = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("::/96"),
}

// Blocked is the default: loopback, link-local (the metadata service),
// private, unspecified and multicast addresses, and blockedNets. A host on
// a private network needs a Private rule.
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsPrivate() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// privateOnly is a private-network address that isn't this machine's or
// link-local: what a Private rule may reach.
func privateOnly(a netip.Addr) bool {
	a = a.Unmap()
	return (a.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(a)) && !a.IsLoopback() && !a.IsLinkLocalUnicast()
}

func (p *Proxy) init() {
	p.once.Do(func() {
		if p.Log == nil {
			p.Log = slog.Default()
		}
		if p.Resolve == nil {
			p.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		if p.Dial == nil {
			d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
			p.Dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
		}
		if p.Blocked == nil {
			p.Blocked = Blocked
		}
		p.slots = make(chan struct{}, maxConns)
		p.tr = &http.Transport{
			DialContext:           func(ctx context.Context, _, addr string) (net.Conn, error) { return p.dial(ctx, addr) },
			Proxy:                 nil,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
		}
	})
}

// errDenied is a destination off the allowlist.
var errDenied = errors.New("not on the allowlist")

// dial connects to host:port if a rule allows it, at an address that
// isn't blocked: the address checked is the one dialed, so a name that
// later resolves elsewhere gains nothing.
func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, fmt.Errorf("%s: %w (addresses never are; allow a host name)", host, errDenied)
	}
	var rule *Rule
	for i := range p.Rules {
		if p.Rules[i].match(host, port) {
			rule = &p.Rules[i]
			break
		}
	}
	if rule == nil {
		return nil, fmt.Errorf("%s: %w", net.JoinHostPort(host, port), errDenied)
	}
	ips, err := p.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	last := fmt.Errorf("%s: no address", host)
	for _, ip := range ips {
		if p.Blocked(ip) && (!rule.Private || !privateOnly(ip)) {
			last = fmt.Errorf("%s resolves to %s, a private or local address: %w", host, ip, errDenied)
			continue
		}
		c, err := p.Dial(ctx, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "factory egress proxy: too many connections", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "factory egress proxy: send requests through it as a proxy", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.fail(w, r.URL.Host, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// tunnelIdle closes a tunnel that has carried nothing either way for
// this long; maxConns bounds the proxy's open requests and tunnels.
var (
	tunnelIdle = 10 * time.Minute
	maxConns   = 1024
)

// errSNI is a TLS ClientHello naming another host than the CONNECT did.
var errSNI = errors.New("sni mismatch")

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	addr := r.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}
	host, _, _ := net.SplitHostPort(addr)
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	up, err := p.dial(r.Context(), addr)
	if err != nil {
		p.fail(w, addr, err)
		return
	}
	defer func() { _ = up.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "can't tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = down.Close() }()
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	// What the client sends first: with TLS, its ClientHello must name the
	// host it asked for, so a tunnel to an allowed name can't carry a
	// session to another name on the same CDN.
	var first bytes.Buffer
	in := io.MultiReader(buf.Reader, down)
	_ = down.SetReadDeadline(time.Now().Add(30 * time.Second))
	sni, isTLS, err := clientHelloSNI(io.TeeReader(in, &first))
	_ = down.SetReadDeadline(time.Time{})
	switch {
	case isTLS && err != nil:
		p.Log.Warn("egress denied", "to", addr, "why", "unreadable TLS ClientHello: "+err.Error())
		return
	case isTLS && strings.TrimSuffix(strings.ToLower(sni), ".") != host:
		p.Log.Warn("egress denied", "to", addr, "why", fmt.Sprintf("TLS names %q, not %s", sni, host))
		return
	case !isTLS && strings.HasSuffix(addr, ":443"):
		p.Log.Warn("egress denied", "to", addr, "why", "not TLS on port 443")
		return
	}
	if _, err := up.Write(first.Bytes()); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { pipe(up, in, down); done <- struct{}{} }()
	go func() { pipe(down, up, up); done <- struct{}{} }()
	<-done
	<-done
}

// pipe copies src to dst until either ends or src is idle for tunnelIdle;
// conn is src's connection, whose read deadline marks idleness.
func pipe(dst net.Conn, src io.Reader, conn net.Conn) {
	b := make([]byte, 32<<10)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(tunnelIdle))
		n, err := src.Read(b)
		if n > 0 {
			if _, werr := dst.Write(b[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	closeWrite(dst)
}

// clientHelloSNI reads a TLS ClientHello from r and returns the server
// name it carries. isTLS is false when r doesn't start a TLS handshake;
// then only the first byte has been read.
func clientHelloSNI(r io.Reader) (sni string, isTLS bool, err error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", false, err
	}
	if b[0] != 0x16 { // handshake record
		return "", false, nil
	}
	conn := &helloConn{r: io.MultiReader(bytes.NewReader(b[:]), r)}
	err = tls.Server(conn, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = h.ServerName
		return nil, errSNI
	}}).Handshake()
	if sni != "" || errors.Is(err, errSNI) {
		return sni, true, nil
	}
	return "", true, err
}

// helloConn feeds a TLS server the client's bytes and accepts no writes:
// the handshake stops once the ClientHello is read.
type helloConn struct {
	net.Conn
	r io.Reader
}

func (c *helloConn) Read(b []byte) (int, error)      { return c.r.Read(b) }
func (c *helloConn) Write([]byte) (int, error)       { return 0, io.ErrClosedPipe }
func (c *helloConn) Close() error                    { return nil }
func (c *helloConn) SetDeadline(time.Time) error     { return nil }
func (c *helloConn) SetReadDeadline(time.Time) error { return nil }
func (c *helloConn) SetWriteDeadline(time.Time) error {
	return nil
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// fail answers a refused or failed request. A refusal says how to allow
// the host, since the agent's error output is where a person sees it.
func (p *Proxy) fail(w http.ResponseWriter, dest string, err error) {
	if errors.Is(err, errDenied) {
		p.Log.Warn("egress denied", "to", dest, "why", err.Error())
		http.Error(w, fmt.Sprintf("factory egress proxy: %v. To let jobs reach it, add it to runner.egress.allow in factory.yaml.", err), http.StatusForbidden)
		return
	}
	p.Log.Info("egress failed", "to", dest, "err", err.Error())
	http.Error(w, "factory egress proxy: "+err.Error(), http.StatusBadGateway)
}

// Serve runs the proxy on addr until ctx ends.
func Serve(ctx context.Context, addr string, p *Proxy) error {
	p.init()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	p.Log.Info("egress proxy listening", "addr", ln.Addr().String(), "rules", len(p.Rules))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
