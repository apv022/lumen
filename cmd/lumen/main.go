// Command lumen runs a personal AI agent (DeepSeek Harness, using Gemini)
// behind a password gate, ready to publish privately over Tor with Sol.
//
//	lumen setup   choose a password and store the Gemini API key
//	lumen         start the gate on 127.0.0.1:7777 and the agent behind it
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/apv022/lumen/internal/gate"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var err error
	switch arg(1) {
	case "", "run":
		err = run()
	case "setup":
		err = setup()
	case "help", "-h", "--help":
		fmt.Println("usage: lumen [setup]")
	default:
		err = fmt.Errorf("unknown command %q (try: lumen setup, lumen)", arg(1))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lumen:", err)
		os.Exit(1)
	}
}

func arg(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return ""
}

func setup() error {
	dir, err := gate.Dir()
	if err != nil {
		return err
	}
	cfg, err := gate.LoadConfig(dir)
	if err != nil {
		cfg = &gate.Config{Listen: "127.0.0.1:7777", DSHVersion: gate.DefaultDSHVersion}
	}
	fmt.Println("Lumen setup. Your password protects the agent when it's reachable over Tor.")
	fmt.Println("Use a long one: anyone with your .onion address can try to guess it.")
	pw, err := readSecret("Password (min 8 chars): ")
	if err != nil {
		return err
	}
	again, err := readSecret("Repeat password: ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("passwords don't match")
	}
	if err := cfg.SetPassword(pw); err != nil {
		return err
	}
	key, err := readSecret("Gemini API key (Enter to keep the current one): ")
	if err != nil {
		return err
	}
	if key != "" {
		cfg.GeminiKey = strings.TrimSpace(key)
	}
	if cfg.GeminiKey == "" {
		fmt.Println("No Gemini key stored. You can also add it later in the agent's Settings → Models.")
	}
	if cfg.Workspace == "" {
		home, _ := os.UserHomeDir()
		cfg.Workspace = filepath.Join(home, "lumen-workspace")
	}
	if err := os.MkdirAll(cfg.Workspace, 0o700); err != nil {
		return err
	}
	if err := cfg.Save(dir); err != nil {
		return err
	}
	fmt.Printf("Saved %s (mode 0600). The agent works in %s.\nNext: run `lumen`.\n", filepath.Join(dir, "config.json"), cfg.Workspace)
	return nil
}

// stdin is shared by every prompt: a new bufio.Reader per prompt would
// swallow buffered input meant for the next one.
var stdin = bufio.NewReader(os.Stdin)

// readSecret reads a line without echo when stdin is a terminal.
func readSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	if exec.Command("stty", "-F", "/dev/tty", "-echo").Run() == nil {
		defer func() {
			exec.Command("stty", "-F", "/dev/tty", "echo").Run()
			fmt.Println()
		}()
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func run() error {
	dir, err := gate.Dir()
	if err != nil {
		return err
	}
	cfg, err := gate.LoadConfig(dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsh := &gate.DSH{Version: cfg.DSHVersion, Home: filepath.Join(dir, "dsh"), Workspace: cfg.Workspace, GeminiKey: cfg.GeminiKey}
	go dsh.Run(ctx)

	ln, err := gate.ListenLoopback(cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: gate.NewServer(cfg, dsh, dir), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	fmt.Printf(`Lumen is running.
  Local:  http://%s
  Agent:  DeepSeek Harness %s (first start downloads it; can take a few minutes)

Reach it from anywhere with Sol: in the Sol panel add a service
  name: lumen   port: %s   protocol: tcp   visibility: "Anyone with Tor Browser"
then open its .onion in Tor Browser on your phone and unlock with your password.
Press Ctrl+C to stop.
`, ln.Addr(), cfg.DSHVersion, portOf(ln.Addr().String()))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}
