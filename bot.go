package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lrstanley/girc"
)

// The trigger must start at byte 0. @Katastrophe and mid-sentence mentions don't match.
func triggered(s, nick string) bool {
	for _, p := range []string{nick + ":", "@" + nick} {
		if len(s) < len(p) || !strings.EqualFold(s[:len(p)], p) {
			continue
		}
		if p[0] != '@' || len(s) == len(p) {
			return true
		}
		r, _ := utf8.DecodeRuneInString(s[len(p):])
		return unicode.IsSpace(r)
	}
	return false
}
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
func replyLines(s string, limit int) []string {
	s = strings.Join(strings.Fields(clean(s)), " ")
	var lines []string
	for len(s) > 0 && len(lines) < limit {
		n := len(s)
		if n > 350 {
			n = 350
			for !utf8.RuneStart(s[n]) {
				n--
			}
			if space := strings.LastIndexByte(s[:n], ' '); space > n/2 {
				n = space
			}
		}
		lines = append(lines, strings.TrimSpace(s[:n]))
		s = strings.TrimSpace(s[n:])
	}
	if s != "" && len(lines) > 0 {
		lines[len(lines)-1] += " …"
	}
	return lines
}

type Bot struct {
	cfg      Config
	ai       *AI
	history  *History
	ready    atomic.Bool
	busy     atomic.Bool
	mu       sync.Mutex
	last     time.Time
	requests sync.WaitGroup
}

func (b *Bot) accept(ctx context.Context, c *girc.Client, e girc.Event, started time.Time) {
	if !b.ready.Load() || e.Source == nil || len(e.Params) != 2 || !e.IsFromChannel() || strings.EqualFold(e.Source.Name, c.GetNick()) {
		return
	}
	if ok, _ := e.IsCTCP(); ok {
		return
	}
	// Ignore all playback batches; this bot does not request historical messages.
	if _, ok := e.Tags.Get("batch"); ok {
		return
	}
	if ts, ok := e.Tags.Get("time"); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil && t.Before(started) {
			return
		}
	}
	ch := ""
	for _, allowed := range b.cfg.IRC.Channels {
		if girc.ToRFC1459(allowed) == girc.ToRFC1459(e.Params[0]) {
			ch = girc.ToRFC1459(allowed)
			break
		}
	}
	if ch == "" {
		return
	}
	msg := e.Last()
	if len(msg) > 4096 {
		return
	}
	// Ordinary conversation is context, but only an explicit prefix causes inference.
	account, _ := e.Tags.Get("account")
	content := fmt.Sprintf("IRC nick=%s account=%s: %s", e.Source.Name, account, clean(msg))
	b.history.add(ch, Message{Role: "user", Content: content})
	if !triggered(msg, b.cfg.IRC.Nick) {
		return
	}
	if len(b.cfg.Bot.AllowedAccounts) > 0 {
		allowed := false
		for _, a := range b.cfg.Bot.AllowedAccounts {
			if account != "" && account != "*" && strings.EqualFold(a, account) {
				allowed = true
			}
		}
		if !allowed {
			return
		}
	}
	b.mu.Lock()
	if time.Since(b.last) < time.Duration(b.cfg.Bot.CooldownSeconds)*time.Second || !b.busy.CompareAndSwap(false, true) {
		b.mu.Unlock()
		c.Cmd.Notice(e.Source.Name, "Busy or cooling down. Please try again shortly.")
		return
	}
	b.last = time.Now()
	b.mu.Unlock()
	snapshot := b.history.snapshot(ch)
	b.requests.Add(1)
	go func() {
		defer b.requests.Done()
		defer b.busy.Store(false)
		reqCtx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.OpenAI.TimeoutSeconds)*time.Second)
		defer cancel()
		answer, err := b.ai.reply(reqCtx, snapshot)
		if ctx.Err() != nil || !c.IsConnected() || !c.IsInChannel(ch) {
			return
		}
		if err != nil {
			slog.Warn("reply failed", "error", err)
			c.Cmd.Message(ch, "Unable to answer: service unavailable or usage limit reached. Check the bot logs.")
			return
		}
		lines := replyLines(answer, b.cfg.Bot.MaxReplyLines)
		for i, line := range lines {
			if i > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(800 * time.Millisecond):
				}
			}
			if !c.IsInChannel(ch) {
				return
			}
			c.Cmd.Message(ch, line)
		}
		b.history.add(ch, Message{Role: "assistant", Content: strings.Join(lines, "\n")})
	}()
}
func (b *Bot) client(ctx context.Context) (*girc.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: b.cfg.IRC.ServerName}
	if b.cfg.IRC.CAFile != "" {
		pem, e := os.ReadFile(b.cfg.IRC.CAFile)
		if e != nil {
			return nil, e
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("invalid IRC CA file")
		}
		tc.RootCAs = roots
	}
	cfg := girc.Config{Server: b.cfg.IRC.Server, Port: b.cfg.IRC.Port, Nick: b.cfg.IRC.Nick, User: "kat", Name: "Configurable IRC assistant", SSL: b.cfg.IRC.TLS, TLSConfig: tc, DisableSTS: true, ServerPass: b.cfg.IRC.Password, Version: "kat-irc 0.1.0", HandleNickCollide: func(string) string { return "" }}
	if b.cfg.IRC.SASLUser != "" {
		cfg.SASL = &girc.SASLPlain{User: b.cfg.IRC.SASLUser, Pass: b.cfg.IRC.SASLPassword}
	}
	c := girc.New(cfg)
	var saslOK atomic.Bool
	var registered atomic.Bool
	var joined sync.Map
	updateReady := func() {
		n := 0
		joined.Range(func(_, _ any) bool { n++; return true })
		b.ready.Store(registered.Load() && n == len(b.cfg.IRC.Channels))
	}
	started := time.Now()
	c.Handlers.Add("903", func(_ *girc.Client, _ girc.Event) { saslOK.Store(true) })
	c.Handlers.Add(girc.CONNECTED, func(c *girc.Client, e girc.Event) {
		if ctx.Err() != nil {
			return
		}
		if cfg.SASL != nil && !saslOK.Load() {
			slog.Error("SASL required but not authenticated")
			go c.Close()
			return
		}
		if len(b.cfg.Bot.AllowedAccounts) > 0 && !c.HasCapability("account-tag") {
			slog.Error("allowed_accounts requires account-tag capability")
			go c.Close()
			return
		}
		if !strings.EqualFold(c.GetNick(), b.cfg.IRC.Nick) {
			slog.Error("server changed bot nick")
			go c.Close()
			return
		}
		registered.Store(true)
		updateReady()
		for _, ch := range b.cfg.IRC.Channels {
			c.Cmd.Join(ch)
		}
	})
	c.Handlers.Add(girc.JOIN, func(c *girc.Client, e girc.Event) {
		if e.Source != nil && strings.EqualFold(e.Source.Name, c.GetNick()) && len(e.Params) > 0 {
			joined.Store(girc.ToRFC1459(e.Params[0]), true)
			updateReady()
		}
	})
	c.Handlers.Add(girc.KICK, func(c *girc.Client, e girc.Event) {
		if len(e.Params) > 1 && strings.EqualFold(e.Params[1], c.GetNick()) {
			joined.Delete(girc.ToRFC1459(e.Params[0]))
			updateReady()
			slog.Warn("bot kicked; reconnecting")
			go c.Close()
		}
	})
	c.Handlers.Add(girc.PART, func(c *girc.Client, e girc.Event) {
		if e.Source != nil && strings.EqualFold(e.Source.Name, c.GetNick()) && len(e.Params) > 0 {
			joined.Delete(girc.ToRFC1459(e.Params[0]))
			updateReady()
			go c.Close()
		}
	})
	c.Handlers.Add(girc.PRIVMSG, func(c *girc.Client, e girc.Event) { b.accept(ctx, c, e, started) })
	for _, numeric := range []string{"433", "432", "471", "473", "474", "475", "476", "477", "489", "904", "905", "906"} {
		c.Handlers.Add(numeric, func(c *girc.Client, e girc.Event) {
			slog.Error("IRC registration/join failed", "numeric", e.Command)
			go c.Close()
		})
	}
	return c, nil
}
func (b *Bot) run(ctx context.Context) error {
	delay := time.Second
	for ctx.Err() == nil {
		session, cancel := context.WithCancel(ctx)
		c, e := b.client(session)
		if e != nil {
			cancel()
			return e
		}
		done := make(chan error, 1)
		go func() { done <- c.Connect() }()
		start := time.Now()
		select {
		case e = <-done:
		case <-ctx.Done():
			c.Close()
			e = <-done
		}
		cancel()
		b.ready.Store(false)
		b.requests.Wait()
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("IRC disconnected; reconnecting", "error", e, "retry_in", delay)
		if time.Since(start) > time.Minute {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
	return nil
}
