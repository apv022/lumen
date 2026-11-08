package gate

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeDSH mimics what matters about DSH: it hands out a cookie for the
// launch token, demands that cookie, rejects non-loopback Origins, and
// echoes over a WebSocket-style upgrade.
func fakeDSH(t *testing.T) (*httptest.Server, chan *http.Request) {
	t.Helper()
	seen := make(chan *http.Request, 16)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "launch-token" {
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-abc", Value: "secret-dsh-cookie", Path: "/", HttpOnly: true})
			http.Redirect(w, r, "./", http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie("dsh-auth-abc"); err != nil || c.Value != "secret-dsh-cookie" {
			http.Error(w, "dsh auth required", http.StatusUnauthorized)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != srv.URL {
			http.Error(w, "fence", http.StatusForbidden)
			return
		}
		select {
		case seen <- r.Clone(r.Context()):
		default:
		}
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			hj, _ := w.(http.Hijacker)
			conn, rw, _ := hj.Hijack()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			go func() { defer conn.Close(); io.Copy(conn, rw) }()
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dsh-auth-abc", Value: "refreshed"})
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<html><head><title>DeepSeek Harness</title></head><body>app</body></html>")
			return
		}
		io.WriteString(w, "dsh ui")
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

type harness struct {
	t      *testing.T
	gate   *httptest.Server
	seen   chan *http.Request
	client *http.Client
}

func init() { pbkdf2Iterations = 1000 } // keep tests fast

func newHarness(t *testing.T) *harness {
	t.Helper()
	backend, seen := fakeDSH(t)
	b, err := exchangeToken(t.Context(), backend.URL, "launch-token")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	if err := cfg.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	d := &DSH{}
	d.set(b, "ready")
	s := NewServer(cfg, d, t.TempDir())
	s.limit = newLimiter(10, time.Hour) // no refill during a test
	g := httptest.NewServer(s)
	t.Cleanup(g.Close)
	jar := newJar()
	return &harness{t: t, gate: g, seen: seen, client: &http.Client{
		Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (h *harness) login(pw string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.gate.URL+loginPath, strings.NewReader(url.Values{"password": {pw}, "next": {"/"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.gate.URL)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (h *harness) get(path string, hdr map[string]string) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.gate.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestUnauthenticatedGetsNothing(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get("/some/page", map[string]string{"Accept": "text/html"})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), loginPath) {
		t.Fatalf("html: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := h.get("/api/sessions", nil)
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(body, "dsh") {
		t.Fatalf("api: %d %q", resp.StatusCode, body)
	}
	select {
	case r := <-h.seen:
		t.Fatalf("unauthenticated request reached DSH: %s", r.URL)
	default:
	}
}

func TestLoginProxyAndCookieIsolation(t *testing.T) {
	h := newHarness(t)
	if resp := h.login("correct horse battery"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	resp, body := h.get("/", map[string]string{"Origin": h.gate.URL, "Cookie": "dsh-auth-abc=forged"})
	if resp.StatusCode != 200 || body != "dsh ui" {
		t.Fatalf("proxied: %d %q", resp.StatusCode, body)
	}
	if sc := resp.Header.Get("Set-Cookie"); strings.Contains(sc, "dsh-auth") {
		t.Fatalf("DSH cookie leaked to browser: %q", sc)
	}
	r := <-h.seen
	if !strings.HasPrefix(r.Host, "127.0.0.1:") || !strings.HasPrefix(r.Header.Get("Origin"), "http://127.0.0.1:") {
		t.Fatalf("Host/Origin not rewritten: %q %q", r.Host, r.Header.Get("Origin"))
	}
	if ck := r.Header.Get("Cookie"); strings.Contains(ck, "lumen_session") || strings.Contains(ck, "forged") {
		t.Fatalf("browser cookies forwarded to DSH: %q", ck)
	}
}

func TestCrossSiteBlocked(t *testing.T) {
	h := newHarness(t)
	h.login("correct horse battery")
	resp, _ := h.get("/api/x", map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site request: %d", resp.StatusCode)
	}
	// A cross-site login POST is refused too (login CSRF).
	req, _ := http.NewRequest(http.MethodPost, h.gate.URL+loginPath, strings.NewReader("password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	resp, _ = h.client.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site login: %d", resp.StatusCode)
	}
}

func TestFirefoxStyleLoginAccepted(t *testing.T) {
	h := newHarness(t)
	// Real browsers send Sec-Fetch-Site; with strict referrer policies they may
	// send Origin: null on form posts. That must still log in.
	req, _ := http.NewRequest(http.MethodPost, h.gate.URL+loginPath, strings.NewReader("password=correct+horse+battery&next=%2F"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("browser login: %d", resp.StatusCode)
	}
	// And a cross-site page posting to us is still refused, whatever Origin says.
	req, _ = http.NewRequest(http.MethodPost, h.gate.URL+loginPath, strings.NewReader("password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.gate.URL)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = h.client.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site with forged Origin: %d", resp.StatusCode)
	}
}

func TestWrongPasswordRateLimited(t *testing.T) {
	h := newHarness(t)
	for i := range 10 {
		if resp := h.login("nope nope nope"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if resp := h.login("correct horse battery"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after burst: %d, want 429", resp.StatusCode)
	}
}

func TestWebSocketPassesThrough(t *testing.T) {
	h := newHarness(t)
	h.login("correct horse battery")
	u, _ := url.Parse(h.gate.URL)
	var cookie string
	for _, c := range h.client.Jar.Cookies(u) {
		cookie = c.Name + "=" + c.Value
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: "+u.Host+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nOrigin: "+h.gate.URL+"\r\nCookie: "+cookie+"\r\n\r\n")
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade status: %q", status)
	}
	for {
		line, _ := br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}
	io.WriteString(conn, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/chat": "/chat", "": "/", "//evil.com": "/", "https://evil.com": "/", "/lumen/logout": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLaunchLineAndRedaction(t *testing.T) {
	line := "dsh web: http://127.0.0.1:41234/?token=abc_DEF-123"
	m := launchRE.FindStringSubmatch(line)
	if m == nil || m[1] != "http://127.0.0.1:41234" || m[2] != "abc_DEF-123" {
		t.Fatalf("parse: %v", m)
	}
	if strings.Contains(redactToken(line), "abc_DEF") {
		t.Fatal("token not redacted in log")
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	if _, err := ListenLoopback("0.0.0.0:0"); err == nil {
		t.Fatal("wildcard bind allowed")
	}
	ln, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

func TestPasswordRules(t *testing.T) {
	c := &Config{}
	if c.SetPassword("short") == nil {
		t.Fatal("short password accepted")
	}
	c.SetPassword("long enough pw")
	if !c.CheckPassword("long enough pw") || c.CheckPassword("long enough pW") {
		t.Fatal("CheckPassword wrong")
	}
}

func TestRebrandHTML(t *testing.T) {
	out := string(rebrandHTML([]byte("<html><head><title>DeepSeek Harness</title></head><body></body></html>")))
	if !strings.Contains(out, "<title>Lumen</title>") || strings.Contains(out, "DeepSeek") {
		t.Fatalf("title not replaced: %s", out)
	}
	if !strings.Contains(out, `/lumen/assets/theme.css?v=`+assetVersion("assets/theme.css")) || !strings.Contains(out, `/lumen/assets/rebrand.js`) ||
		strings.Index(out, "theme.css") > strings.Index(out, "</head>") {
		t.Fatalf("theme not injected into head: %s", out)
	}
}

func TestProxiedPagesAreRebranded(t *testing.T) {
	h := newHarness(t)
	h.login("correct horse battery")
	resp, body := h.get("/", map[string]string{"Accept": "text/html", "Accept-Encoding": "gzip, br"})
	if resp.StatusCode != 200 || !strings.Contains(body, "theme.css") || strings.Contains(body, "DeepSeek Harness") {
		t.Fatalf("page not rebranded: %d %q", resp.StatusCode, body)
	}
	// The browser's own encodings must not be forwarded for HTML (Go's client
	// may add gzip itself, which it then decodes transparently).
	if r := <-h.seen; strings.Contains(r.Header.Get("Accept-Encoding"), "br") {
		t.Fatalf("browser encodings forwarded for HTML: %q", r.Header.Get("Accept-Encoding"))
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("rebranded page marked as encoded: %q", resp.Header.Get("Content-Encoding"))
	}
}

func TestBrandAssetsReplaceDSH(t *testing.T) {
	h := newHarness(t)
	for path, ctype := range map[string]string{
		"/favicon.svg": "image/svg+xml", "/apple-touch-icon.png": "image/png", "/manifest.webmanifest": "application/manifest+json",
	} {
		resp, body := h.get(path, nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctype || len(body) == 0 {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	_, man := h.get("/manifest.webmanifest", nil)
	if !strings.Contains(man, `"name": "Lumen"`) {
		t.Fatalf("manifest: %s", man)
	}
	if png := appIconPNG(); len(png) < 100 || string(png[1:4]) != "PNG" {
		t.Fatal("app icon is not a PNG")
	}
}
