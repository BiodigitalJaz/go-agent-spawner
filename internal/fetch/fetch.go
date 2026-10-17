// Package fetch downloads web pages as plain text for agents, which cannot
// browse. It is an SSRF boundary: by default it refuses any address that is
// not public (private, loopback, link-local, CGNAT, multicast...). Addresses
// are checked when connecting, after DNS resolution and on every redirect,
// so neither DNS rebinding nor a redirect can reach an internal service.
// Internal fetching can be allowed (an internal research deployment); every
// attempt on an internal address is logged, allowed or not.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	MaxURLs      = 5
	maxBytes     = 5 << 20
	timeout      = 20 * time.Second
	maxRedirects = 5
)

// ErrInternal means the URL resolved to a non-public address and internal
// fetching is not allowed.
var ErrInternal = errors.New("internal address")

type Page struct {
	URL   string // final URL after redirects
	Title string
	Text  string
	Bytes int
}

type Fetcher struct {
	log           *slog.Logger
	allowInternal bool
	resolve       func(ctx context.Context, host string) ([]netip.Addr, error)
	internal      func(netip.Addr) bool // Internal; replaceable in tests
	client        *http.Client
}

// New returns a Fetcher; allowInternal lets it reach non-public addresses.
func New(log *slog.Logger, allowInternal bool) *Fetcher {
	f := &Fetcher{log: log, allowInternal: allowInternal, internal: Internal}
	f.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	f.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // a proxy would hide the real destination from the check
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				ip, err := f.check(ctx, addr)
				if err != nil {
					return nil, err
				}
				_, port, _ := net.SplitHostPort(addr)
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			},
			TLSHandshakeTimeout:    10 * time.Second,
			ResponseHeaderTimeout:  15 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
	return f
}

// check resolves addr's host and returns the address to dial, refusing
// internal ones unless allowed.
func (f *Fetcher) check(ctx context.Context, addr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return netip.Addr{}, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = f.resolve(ctx, host); err != nil {
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if f.internal(ip) {
			if !f.allowInternal {
				f.log.Warn("fetch blocked: internal address", "host", host, "ip", ip.String())
				return netip.Addr{}, fmt.Errorf("%w: %s resolves to %s (an admin can allow internal fetching in Settings)", ErrInternal, host, ip)
			}
			f.log.Info("fetch of internal address allowed by settings", "host", host, "ip", ip.String())
		}
	}
	return ips[0], nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Internal reports addresses that are not on the public internet.
func Internal(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip) ||
		(ip.Is4() && ip.As4()[0] == 0) // 0.0.0.0/8
}

// Fetch downloads one URL and returns its text.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (*Page, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("not a web URL: %q", raw)
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agent-spawner-fetch/1 (research agent; text extraction)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,text/markdown,application/json;q=0.9,*/*;q=0.5")
	resp, err := f.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && errors.Is(ue.Err, ErrInternal) {
			return nil, ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s returned HTTP %d", u.Host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("page larger than %d MB", maxBytes>>20)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	p := &Page{URL: resp.Request.URL.String(), Bytes: len(body)}
	switch {
	case ct == "text/html" || ct == "application/xhtml+xml" || (ct == "" && looksHTML(body)):
		p.Title, p.Text = HTMLText(string(body))
	case strings.HasPrefix(ct, "text/") || ct == "application/json" || ct == "application/xml" || strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml"):
		p.Text = strings.TrimSpace(string(body))
	default:
		return nil, fmt.Errorf("unsupported content type %q", ct)
	}
	f.log.Info("fetched", "url", p.URL, "status", resp.StatusCode, "bytes", p.Bytes, "text_chars", len(p.Text), "ms", time.Since(start).Milliseconds())
	return p, nil
}

func looksHTML(b []byte) bool {
	s := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(s, "<html") || strings.Contains(s, "<!doctype html")
}
