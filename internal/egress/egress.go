// Package egress is the proxy every job's traffic leaves through. Jobs sit
// on a Docker network with no way out; the proxy, on that network and on
// one that does, forwards only to the hosts on its allowlist: the model
// APIs, the git host and package registries. A builder that reads a
// prompt injection can't send what it holds anywhere else.
package egress

import (
	"context"
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
// and sign-in, GitHub and GitLab, and the common package registries. None
// is a place anyone can upload to without credentials the job lacks.
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

	once sync.Once
	tr   *http.Transport
}

// Blocked is the default: loopback, link-local (the metadata service),
// private, shared and unspecified addresses, and multicast. A host on a
// private network needs a Private rule.
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsPrivate() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a) // carrier-grade NAT, Tailscale
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

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	addr := r.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}
	up, err := p.dial(r.Context(), addr)
	if err != nil {
		p.fail(w, addr, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "can't tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_, _ = down.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() {
		// Bytes the client sent after the CONNECT, then the rest.
		if r := buf.Reader; r.Buffered() > 0 {
			b, _ := r.Peek(r.Buffered())
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, down)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(down, up)
		closeWrite(down)
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = up.Close()
	_ = down.Close()
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
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
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
