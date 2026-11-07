package gate

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "lumen_session"
	sessionTTL    = 30 * 24 * time.Hour
	loginPath     = "/lumen/login"
)

//go:embed assets
var assetFS embed.FS

var loginTmpl = template.Must(template.ParseFS(assetFS, "assets/login.html"))

// Server is the gate's HTTP handler.
type Server struct {
	cfg   *Config
	dsh   *DSH
	dir   string
	limit *limiter

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

// NewServer builds the gate. Sessions are persisted in dir so restarting
// Lumen doesn't log every phone out.
func NewServer(cfg *Config, dsh *DSH, dir string) *Server {
	s := &Server{cfg: cfg, dsh: dsh, dir: dir, limit: newLimiter(10, 10*time.Second), sessions: map[string]time.Time{}}
	s.loadSessions()
	return s
}

func (s *Server) sessionsPath() string { return filepath.Join(s.dir, "sessions.json") }

func (s *Server) loadSessions() {
	data, err := os.ReadFile(s.sessionsPath())
	if err != nil {
		return
	}
	var m map[string]time.Time
	if json.Unmarshal(data, &m) == nil {
		now := time.Now()
		for k, v := range m {
			if v.After(now) {
				s.sessions[k] = v
			}
		}
	}
}

func (s *Server) saveSessionsLocked() {
	data, _ := json.Marshal(s.sessions)
	tmp := s.sessionsPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, s.sessionsPath())
	}
}

func (s *Server) newSession() string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(sessionTTL)
	s.saveSessionsLocked()
	s.mu.Unlock()
	return tok
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, c.Value)
		s.saveSessionsLocked()
		return false
	}
	return true
}

func (s *Server) dropSession(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.saveSessionsLocked()
		s.mu.Unlock()
	}
}

// ServeHTTP routes /lumen/* to the gate and everything else to DSH.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == loginPath:
		s.login(w, r)
	case r.URL.Path == "/lumen/logout":
		s.logout(w, r)
	case strings.HasPrefix(r.URL.Path, "/lumen/assets/"):
		sub, _ := fs.Sub(assetFS, "assets")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.StripPrefix("/lumen/assets/", http.FileServerFS(sub)).ServeHTTP(w, r)
	default:
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && serveBrand(w, r) {
			return
		}
		s.proxy(w, r)
	}
}

// sameOrigin enforces the cross-site check DSH would do itself.
//
// Browsers mark every request with Sec-Fetch-Site, which pages cannot forge;
// when present it decides. Otherwise the Origin header must match the gate's
// own host. Requests with neither (curl, old browsers) are allowed only as
// safe reads.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none": // "none" = typed URL or bookmark
		return true
	case "same-site", "cross-site":
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && !isUpgrade(r) && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin") // no-referrer would make browsers send Origin: null
	h.Set("X-Frame-Options", "DENY")
}

type loginView struct {
	Error string
	Next  string
	Wait  int
}

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/lumen/") {
		return "/"
	}
	return next
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; manifest-src 'self'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	view := loginView{Next: safeNext(r.URL.Query().Get("next"))}
	if r.Method == http.MethodGet {
		if s.validSession(r) {
			http.Redirect(w, r, view.Next, http.StatusSeeOther)
			return
		}
		loginTmpl.Execute(w, view)
		return
	}
	if r.Method != http.MethodPost || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	view.Next = safeNext(r.FormValue("next"))
	if wait := s.limit.wait(); wait > 0 {
		view.Error = "Too many attempts. Try again shortly."
		view.Wait = int(wait.Seconds()) + 1
		w.WriteHeader(http.StatusTooManyRequests)
		loginTmpl.Execute(w, view)
		return
	}
	if !s.cfg.CheckPassword(r.FormValue("password")) {
		s.limit.fail()
		slog.Warn("failed login")
		view.Error = "That password isn't right."
		w.WriteHeader(http.StatusUnauthorized)
		loginTmpl.Execute(w, view)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.newSession(), Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, // Origin checks guard writes
		Secure: r.TLS != nil,
	})
	slog.Info("login")
	// 303 back to where the user was going.
	http.Redirect(w, r, view.Next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !sameOrigin(r) {
		http.Error(w, "use the logout button", http.StatusMethodNotAllowed)
		return
	}
	s.dropSession(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if !s.validSession(r) {
		if wantsHTML(r) {
			http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return
	}
	b, status := s.dsh.Current()
	if b == nil {
		w.Header().Set("Retry-After", "3")
		if wantsHTML(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			startingPage.Execute(w, status)
			return
		}
		http.Error(w, "agent is "+status, http.StatusServiceUnavailable)
		return
	}
	backendOrigin := b.Base.Scheme + "://" + b.Base.Host
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(b.Base)
			pr.Out.Host = b.Base.Host
			// DSH's fence only trusts its own loopback origin; the gate has
			// already checked the real Origin above.
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", backendOrigin)
			}
			if pr.In.Header.Get("Referer") != "" {
				pr.Out.Header.Set("Referer", backendOrigin+"/")
			}
			// Ask for uncompressed HTML so the Lumen theme can be injected;
			// everything else (the big JS bundles) stays compressed.
			if wantsHTML(pr.In) {
				pr.Out.Header.Del("Accept-Encoding")
			}
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("Forwarded")
			// Replace browser cookies with DSH's own session cookie.
			pr.Out.Header.Set("Cookie", b.Cookie)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie") // DSH's cookies stay server-side
			if isUpgrade(resp.Request) {
				slog.Info("websocket", "path", resp.Request.URL.Path, "status", resp.StatusCode)
				return nil
			}
			return rewriteHTMLResponse(resp)
		},
		FlushInterval: -1, // stream SSE and long responses immediately
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("proxy error", "path", r.URL.Path, "upgrade", isUpgrade(r), "err", err)
			http.Error(w, "the agent is not reachable right now", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

var startingPage = template.Must(template.New("starting").Parse(`<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><meta http-equiv="refresh" content="3">
<link rel="stylesheet" href="/lumen/assets/lumen.css"><title>Lumen · starting</title>
<div class="sky" aria-hidden="true"><span></span><span></span><span></span></div><main class="center"><div class="card"><img class="eye" src="/lumen/assets/mark.svg" alt=""><h1>Lumen is waking up</h1>
<p class="muted">Status: {{.}}. The first start downloads the agent and can take a few minutes. This page refreshes by itself.</p></div></main>`))

// limiter is a global token bucket for failed logins. Behind Tor every
// client shares an address, so limits are global: an attacker gets about one
// guess per refill interval, while a real user rarely notices.
type limiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	every  time.Duration
	last   time.Time
}

func newLimiter(burst int, every time.Duration) *limiter {
	return &limiter{tokens: float64(burst), burst: float64(burst), every: every, last: time.Now()}
}

func (l *limiter) refill() {
	now := time.Now()
	l.tokens = min(l.burst, l.tokens+float64(now.Sub(l.last))/float64(l.every))
	l.last = now
}

// wait returns how long until another attempt is allowed (0 = now).
func (l *limiter) wait() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	if l.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - l.tokens) * float64(l.every))
}

func (l *limiter) fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	l.tokens = max(0, l.tokens-1)
}

// ListenLoopback refuses non-loopback addresses: Sol is the only way in.
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, &net.AddrError{Err: "Lumen only listens on loopback; publish it with Sol", Addr: addr}
	}
	return net.Listen("tcp", addr)
}
