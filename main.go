package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("kat-irc stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	command, args := "serve", os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("kat-irc "+command, flag.ContinueOnError)
	configPath := fs.String("config", "config.json", "configuration file")
	credentials := fs.String("credentials", "oauth.json", "OAuth file (login only)")
	port := fs.Int("port", 1455, "loopback OAuth callback port (login only)")
	health := fs.String("health-listen", ":8080", "health HTTP address; empty disables it (serve only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch command {
	case "login":
		unlock, err := lockFile(*credentials)
		if err != nil {
			return err
		}
		defer unlock()
		return login(ctx, *credentials, *port)
	case "serve":
		return serve(ctx, *configPath, *health, 2*time.Second)
	case "models", "check":
		c, err := loadConfig(*configPath)
		if err != nil {
			return err
		}
		if command == "check" {
			fmt.Println("Configuration valid (credentials and network not checked)")
			return nil
		}
		a, err := newAI(c)
		if err != nil {
			return err
		}
		return a.models(ctx)
	default:
		return fmt.Errorf("commands: serve, login, models, check")
	}
}

func healthHandler(current *atomic.Pointer[Bot]) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		b := current.Load()
		if b == nil || !b.ready.Load() {
			http.Error(w, "waiting for setup or IRC connection", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// Waiting is an application state, not a crash. A missing/invalid setup never connects to IRC.
// Once started, configuration/persona changes require restarting serve; OAuth is reloaded per request.
func waitForSetup(ctx context.Context, path string, interval time.Duration) (*Bot, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	previous := ""
	for {
		c, err := loadConfig(path)
		stage := "configuration and persona/memory files"
		var a *AI
		var h *History
		if err == nil {
			stage = "OpenAI credentials (run login or set openai.api_key)"
			a, err = newAI(c)
		}
		if err == nil {
			stage = "history file"
			h, err = newHistory(c.Bot.HistoryFile, c.Bot.HistoryMessages)
		}
		if err == nil {
			return &Bot{cfg: c, ai: a, history: h, own: newOwnMessages(50)}, nil
		}
		// Avoid logging configuration values, tokens, or file contents.
		if stage != previous {
			slog.Info("waiting for setup", "needs", stage, "hint", "use kat-irc check -config to validate configuration")
			previous = stage
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func serve(parent context.Context, path, health string, interval time.Duration) error {
	// Separate from the OAuth lock so an operator can log in while serve is running.
	unlock, err := lockFile(path + ".serve")
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var current atomic.Pointer[Bot]
	serverErrors := make(chan error, 1)
	if health != "" {
		ln, err := net.Listen("tcp", health)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: healthHandler(&current), ReadHeaderTimeout: 5 * time.Second}
		defer srv.Close()
		go func() {
			err := srv.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrors <- err
				cancel()
			}
		}()
	}
	b, err := waitForSetup(ctx, path, interval)
	if err == nil {
		current.Store(b)
		saved := make(chan struct{})
		go func() {
			defer close(saved)
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if e := b.history.save(); e != nil {
						slog.Error("save history", "error", e)
					}
				}
			}
		}()
		slog.Info("IRC bot starting", "auth", b.cfg.OpenAI.Auth, "model", b.cfg.OpenAI.Model, "tls", b.cfg.IRC.TLS)
		err = b.run(ctx)
		cancel()
		<-saved
		if e := b.history.save(); e != nil {
			slog.Error("save history", "error", e)
		}
	}
	select {
	case e := <-serverErrors:
		return e
	default:
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

var errLocked = errors.New("file is busy in another kat-irc process")

// flock protects rotating credentials and local singleton processes on Linux/macOS.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func waitLock(ctx context.Context, path string) (func(), error) {
	for {
		unlock, err := lockFile(path)
		if !errors.Is(err, errLocked) {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
