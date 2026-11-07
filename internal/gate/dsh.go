package gate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// launchRE matches the line DSH prints on startup, e.g.
// "dsh web: http://127.0.0.1:41234/?token=abc...".
var launchRE = regexp.MustCompile(`dsh web: (http://127\.0\.0\.1:\d+)/\?token=([A-Za-z0-9_-]+)`)

// Backend is where DSH is listening and the cookie that authenticates us to it.
type Backend struct {
	Base   *url.URL // http://127.0.0.1:<port>
	Cookie string   // "name=value" of DSH's session cookie
}

// DSH supervises the DeepSeek Harness web server as a child process.
type DSH struct {
	Version   string
	Home      string // DSH_HOME, keeps DSH's data separate from any other install
	Workspace string
	GeminiKey string
	// Command overrides the launcher (tests); default is npx.
	Command func(ctx context.Context) *exec.Cmd

	mu      sync.RWMutex
	backend *Backend
	status  string
}

// Current returns the live backend, or nil while DSH is (re)starting.
func (d *DSH) Current() (*Backend, string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.backend, d.status
}

func (d *DSH) set(b *Backend, status string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.backend, d.status = b, status
}

func (d *DSH) command(ctx context.Context) *exec.Cmd {
	if d.Command != nil {
		return d.Command(ctx)
	}
	cmd := exec.CommandContext(ctx, "npx", "-y", "@deepseek-ai/dsh@"+d.Version,
		"web", "--no-open", "--host", "127.0.0.1", "--port", "0")
	env := os.Environ()
	// Prefer a Node 24 installed in ~/.local/node (DSH needs Node >= 24.9).
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".local/node/bin/node")); err == nil {
			env = append(env, "PATH="+filepath.Join(home, ".local/node/bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	env = append(env, "DSH_HOME="+d.Home)
	if d.GeminiKey != "" {
		env = append(env, "GEMINI_API_KEY="+d.GeminiKey)
	}
	cmd.Env = env
	cmd.Dir = d.Workspace
	return cmd
}

// Run keeps DSH running until ctx is cancelled, restarting it with back-off.
func (d *DSH) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		d.set(nil, "starting the agent")
		started := time.Now()
		err := d.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		d.set(nil, "the agent stopped; restarting")
		slog.Warn("dsh exited", "err", err)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (d *DSH) runOnce(ctx context.Context) error {
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		return err
	}
	cmd := d.command(ctx)
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	logf, _ := os.OpenFile(filepath.Join(d.Home, "dsh.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if logf != nil {
		defer logf.Close()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting DSH (is Node 24 installed?): %w", err)
	}
	defer killGroup(cmd)

	found := make(chan [2]string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if logf != nil {
				io.WriteString(logf, redactToken(line)+"\n")
			}
			if m := launchRE.FindStringSubmatch(line); m != nil {
				select {
				case found <- [2]string{m[1], m[2]}:
				default:
				}
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case m := <-found:
		b, err := exchangeToken(ctx, m[0], m[1])
		if err != nil {
			return err
		}
		d.set(b, "ready")
		slog.Info("agent ready", "backend", b.Base.String())
	case err := <-done:
		return fmt.Errorf("DSH exited before it was ready: %v (log: %s)", err, filepath.Join(d.Home, "dsh.log"))
	case <-time.After(10 * time.Minute): // first run downloads DSH
		return errors.New("DSH did not start within 10 minutes")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// exchangeToken trades DSH's launch token for its session cookie, exactly
// as a browser opening the printed URL would.
func exchangeToken(ctx context.Context, base, token string) (*Backend, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/?token="+url.QueryEscape(token), nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DSH token exchange: %w", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, "dsh-auth") {
			return &Backend{Base: u, Cookie: c.Name + "=" + c.Value}, nil
		}
	}
	return nil, fmt.Errorf("DSH token exchange: no session cookie (status %d)", resp.StatusCode)
}

func redactToken(line string) string {
	return launchRE.ReplaceAllString(line, "dsh web: $1/?token=[redacted]")
}
