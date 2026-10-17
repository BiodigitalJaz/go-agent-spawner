package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestInternal(t *testing.T) {
	for _, s := range []string{"10.1.2.3", "172.16.0.1", "192.168.1.64", "127.0.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "224.0.0.1"} {
		if !Internal(netip.MustParseAddr(s)) {
			t.Errorf("%s should be internal", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "100.128.0.1"} {
		if Internal(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
}

func TestHTMLText(t *testing.T) {
	title, text := HTMLText(`<!doctype html><html><head><title> Antrado </title><style>.x{}</style><script>var a=1</script></head>
<body><nav><a href="/">Home</a></nav><svg><title>How an order is contained</title></svg><h1>We do not claim it makes money</h1><p>Plain   words,
 one idea.</p><ul><li>First</li><li>Second <b>point</b></li></ul><img alt="risk refusal" src="x.png">
<noscript>enable js</noscript><pre>code  stays
  as is</pre><table><tr><th>Plan</th><td>Fee</td></tr></table></body></html>`)
	if title != "Antrado" {
		t.Fatalf("title %q", title)
	}
	for _, want := range []string{"# We do not claim it makes money", "Plain words, one idea.", "- First", "- Second point", "[image: risk refusal]", "code  stays\n  as is", "| Plan | Fee"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"var a=1", ".x{}", "enable js", "How an order is contained"} {
		if strings.Contains(text, bad) {
			t.Errorf("text should not contain %q", bad)
		}
	}
}

// serve starts a server on a specific loopback address.
func serve(t *testing.T, ip string, h http.Handler) string {
	t.Helper()
	l, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return s.URL
}

func newTest(allow bool, logs *bytes.Buffer) *Fetcher {
	f := New(slog.New(slog.NewTextHandler(logs, nil)), allow)
	// 127.0.0.1 plays a public host, 127.0.0.2 an internal one.
	f.internal = func(ip netip.Addr) bool { return ip == netip.MustParseAddr("127.0.0.2") }
	return f
}

func TestBlocksInternalAndLogs(t *testing.T) {
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<title>Inside</title><p>secret dashboard</p>")
	})
	internal := serve(t, "127.0.0.2", page)
	public := serve(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, internal+"/", http.StatusFound) // a public page bouncing inward
			return
		}
		page(w, r)
	}))
	ctx := context.Background()

	var logs bytes.Buffer
	f := newTest(false, &logs)
	if p, err := f.Fetch(ctx, public+"/"); err != nil || p.Title != "Inside" || !strings.Contains(p.Text, "secret dashboard") {
		t.Fatalf("public fetch: %+v %v", p, err)
	}
	if _, err := f.Fetch(ctx, internal+"/"); !errors.Is(err, ErrInternal) {
		t.Fatalf("internal fetch must be blocked, got %v", err)
	}
	if _, err := f.Fetch(ctx, public+"/hop"); !errors.Is(err, ErrInternal) {
		t.Fatalf("a redirect to an internal address must be blocked, got %v", err)
	}
	if n := strings.Count(logs.String(), "fetch blocked: internal address"); n != 2 {
		t.Fatalf("each blocked attempt must be logged, got %d:\n%s", n, logs.String())
	}

	logs.Reset()
	f = newTest(true, &logs)
	if p, err := f.Fetch(ctx, internal+"/"); err != nil || p.Title != "Inside" {
		t.Fatalf("allowed internal fetch: %+v %v", p, err)
	}
	if !strings.Contains(logs.String(), "fetch of internal address allowed by settings") {
		t.Fatalf("allowed internal attempts must be logged too:\n%s", logs.String())
	}
}

func TestRejectsBadInput(t *testing.T) {
	f := newTest(false, &bytes.Buffer{})
	for _, u := range []string{"ftp://example.com/x", "file:///etc/passwd", "javascript:alert(1)", "not a url"} {
		if _, err := f.Fetch(context.Background(), u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
	bin := serve(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 1, 2})
	}))
	if _, err := f.Fetch(context.Background(), bin); err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Errorf("binary content should be rejected, got %v", err)
	}
}
