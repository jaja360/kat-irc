package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

// validReaction accepts a short, single-token emoji suitable for an IRCv3 tag.
func validReaction(s string) bool {
	if s == "" || len(s) > 32 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// escapeTagValue applies IRCv3 message-tag value escaping; CR/LF are dropped.
func escapeTagValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case ';':
			b.WriteString(`\:`)
		case ' ':
			b.WriteString(`\s`)
		case '\r', '\n':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// logNote returns a single-line, bounded form of model commentary for logs.
// It is never sent to IRC or kept in the conversation history.
func logNote(s string) string {
	s = strings.Join(strings.Fields(clean(s)), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
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
	cfg         Config
	ai          *AI
	history     *History
	own         *ownMessages
	members     *memberList
	filehostURL atomic.Value // string, from the 005 soju.im/FILEHOST token
	ready       atomic.Bool
	busy        atomic.Bool
	reacting    atomic.Bool
	mu          sync.Mutex
	last        time.Time
	lastReact   time.Time
	requests    sync.WaitGroup
}

// ownMessages tracks the bot's own recent message ids per channel (echo-message).
type ownMessages struct {
	mu    sync.Mutex
	limit int
	ids   map[string][]ownMsg
}

type ownMsg struct {
	id   string
	text string
}

func newOwnMessages(limit int) *ownMessages {
	return &ownMessages{limit: limit, ids: map[string][]ownMsg{}}
}

func (o *ownMessages) add(ch, id, text string) {
	if o == nil || id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	v := append(o.ids[ch], ownMsg{id: id, text: text})
	if len(v) > o.limit {
		v = v[len(v)-o.limit:]
	}
	o.ids[ch] = v
}

func (o *ownMessages) has(ch, id string) bool {
	if o == nil || id == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range o.ids[ch] {
		if m.id == id {
			return true
		}
	}
	return false
}

func (o *ownMessages) recent(ch string, n int) []ownMsg {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	v := o.ids[ch]
	if len(v) > n {
		v = v[len(v)-n:]
	}
	return append([]ownMsg(nil), v...)
}

// allowedChannel returns the canonical watched channel for target, or "".
func (b *Bot) allowedChannel(target string) string {
	for _, allowed := range b.cfg.IRC.Channels {
		if girc.ToRFC1459(allowed) == girc.ToRFC1459(target) {
			return girc.ToRFC1459(allowed)
		}
	}
	return ""
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
	ch := b.allowedChannel(e.Params[0])
	if ch == "" {
		return
	}
	msg := e.Last()
	if len(msg) > 4096 {
		return
	}
	// Ordinary conversation is context, but only an explicit prefix causes inference.
	account, _ := e.Tags.Get("account")
	msgid, _ := e.Tags.Get("msgid")
	content := fmt.Sprintf("IRC nick=%s account=%s: %s", e.Source.Name, account, clean(msg))
	b.history.add(ch, Message{Role: "user", Content: content, MsgID: msgid})
	if !triggered(msg, b.cfg.IRC.Nick) {
		b.maybeReact(ctx, c, ch)
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
	slog.Info("answering", "channel", ch, "nick", e.Source.Name, "message", logNote(msg))
	snapshot := b.history.snapshot(ch)
	b.requests.Add(1)
	go func() {
		defer b.requests.Done()
		defer b.busy.Store(false)
		b.setTyping(c, ch, "active")
		typingCleared := false
		clearTyping := func() {
			if typingCleared {
				return
			}
			typingCleared = true
			b.setTyping(c, ch, "done")
		}
		defer clearTyping()
		reqCtx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.OpenAI.TimeoutSeconds)*time.Second)
		defer cancel()
		opts := replyOptions{presence: b.presenceSummary(c, ch)}
		if b.cfg.Bot.Redaction && c.HasCapability("echo-message") && c.HasCapability("draft/message-redaction") {
			opts.redact = true
			opts.ownMessages = b.ownSummary(ch)
		}
		rep, err := b.ai.reply(reqCtx, snapshot, opts)
		if ctx.Err() != nil || !c.IsConnected() || !c.IsInChannel(ch) {
			return
		}
		if err != nil {
			slog.Warn("reply failed", "error", err)
			c.Cmd.Message(ch, "Unable to answer: service unavailable or usage limit reached. Check the bot logs.")
			return
		}
		reactions := b.applyReactions(c, ch, rep.Calls)
		redactions := b.applyRedactions(c, ch, rep.Calls)
		replyTo, text := "", rep.Text
		switch b.cfg.Bot.ReplyThreading {
		case "always":
			replyTo = msgid
		case "model":
			replyTo, text = b.threadedReply(ch, text)
		}
		lines := replyLines(text, b.cfg.Bot.MaxReplyLines)
		lines = append(lines, b.applyImages(ctx, c, ch, rep.Calls)...)
		if len(lines) == 0 {
			switch {
			case b.cfg.Bot.Images.Enabled && hasToolCall(rep.Calls, imageToolName):
				c.Cmd.Message(ch, "I couldn't generate that image. Check the bot logs.")
			case reactions == 0 && redactions == 0:
				slog.Warn("model produced no answer", "channel", ch, "tools", logTools(rep.Calls))
			}
			return
		}
		clearTyping()
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
			b.sendAnswer(c, ch, line, replyTo, i == 0)
		}
		b.history.add(ch, Message{Role: "assistant", Content: strings.Join(lines, "\n")})
	}()
}

// applyReactions emits reactions, accepting only msgids seen in this channel.
func (b *Bot) applyReactions(c *girc.Client, ch string, calls []ToolCall) int {
	if len(calls) == 0 || !b.cfg.Bot.Reactions.Enabled {
		return 0
	}
	if !c.HasCapability("message-tags") {
		slog.Warn("reactions unavailable: server did not negotiate message-tags")
		return 0
	}
	max := b.cfg.Bot.Reactions.MaxPerReply
	sent := 0
	for _, call := range calls {
		if sent >= max {
			break
		}
		if call.Name != reactToolName {
			continue
		}
		var args struct {
			Emoji  string `json:"emoji"`
			MsgID  string `json:"msgid"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			slog.Warn("ignoring malformed reaction arguments", "error", err)
			continue
		}
		emoji := strings.TrimSpace(args.Emoji)
		if !validReaction(emoji) {
			slog.Warn("ignoring invalid reaction emoji", "emoji", args.Emoji)
			continue
		}
		if !b.history.hasMsgID(ch, args.MsgID) {
			slog.Warn("ignoring reaction to unknown msgid", "msgid", args.MsgID)
			continue
		}
		raw := fmt.Sprintf("@+draft/react=%s;+reply=%s TAGMSG %s", escapeTagValue(emoji), escapeTagValue(args.MsgID), ch)
		if err := c.Cmd.SendRawNoSplit(raw); err != nil {
			slog.Warn("sending reaction failed", "error", err)
			continue
		}
		sent++
		slog.Info("reacted", "channel", ch, "emoji", emoji, "msgid", args.MsgID, "reason", logNote(args.Reason))
		// Remember the action so the model does not react to the same message twice.
		b.history.add(ch, Message{Role: "assistant", Content: fmt.Sprintf("[reacted %s to msgid=%s]", emoji, args.MsgID)})
	}
	return sent
}

// applyImages generates and uploads image tool calls, returning URLs to post.
func (b *Bot) applyImages(ctx context.Context, c *girc.Client, ch string, calls []ToolCall) []string {
	if !b.cfg.Bot.Images.Enabled || len(calls) == 0 {
		return nil
	}
	max := b.cfg.Bot.Images.MaxPerReply
	if max < 1 {
		max = 1
	}
	var urls []string
	for _, call := range calls {
		if len(urls) >= max {
			break
		}
		if call.Name != imageToolName {
			continue
		}
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			slog.Warn("ignoring malformed image arguments", "error", err)
			continue
		}
		prompt := strings.TrimSpace(clean(args.Prompt))
		if prompt == "" || len(prompt) > 1000 {
			slog.Warn("ignoring invalid image prompt")
			continue
		}
		slog.Info("generating image", "channel", ch, "prompt", logNote(prompt))
		imgCtx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.Bot.Images.TimeoutSeconds)*time.Second)
		data, err := b.ai.generateImage(imgCtx, prompt)
		if err == nil {
			var u string
			if u, err = b.uploadImage(imgCtx, c, data); err == nil {
				urls = append(urls, u)
				slog.Info("image posted", "channel", ch, "bytes", len(data), "url", u)
			}
		}
		cancel()
		if err != nil {
			slog.Warn("image generation or upload failed", "error", err)
		}
	}
	return urls
}

// applyRedactions retracts one of the bot's own tracked messages, returning how many.
func (b *Bot) applyRedactions(c *girc.Client, ch string, calls []ToolCall) int {
	if !b.cfg.Bot.Redaction || b.own == nil || len(calls) == 0 {
		return 0
	}
	for _, call := range calls {
		if call.Name != redactToolName {
			continue
		}
		var args struct {
			MsgID  string `json:"msgid"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			slog.Warn("ignoring malformed redact arguments", "error", err)
			return 0
		}
		if !b.own.has(ch, args.MsgID) {
			slog.Warn("ignoring redact of unknown or foreign msgid", "msgid", args.MsgID)
			return 0
		}
		raw := fmt.Sprintf("REDACT %s %s", ch, args.MsgID)
		if reason := logNote(args.Reason); reason != "" {
			raw += " :" + reason
		}
		if err := c.Cmd.SendRawNoSplit(raw); err != nil {
			slog.Warn("redaction failed", "error", err)
			return 0
		}
		slog.Info("redacted own message", "channel", ch, "msgid", args.MsgID, "reason", logNote(args.Reason))
		b.history.add(ch, Message{Role: "assistant", Content: fmt.Sprintf("[redacted msgid=%s]", args.MsgID)})
		return 1
	}
	return 0
}

func hasToolCall(calls []ToolCall, name string) bool {
	for _, call := range calls {
		if call.Name == name {
			return true
		}
	}
	return false
}

// threadedReply extracts a leading [[reply:<msgid>]] marker from the answer,
// returning the target id (only if seen in this channel) and the answer text.
func (b *Bot) threadedReply(ch, text string) (string, string) {
	s := strings.TrimSpace(text)
	const prefix = "[[reply:"
	if !strings.HasPrefix(s, prefix) {
		return "", text
	}
	end := strings.Index(s, "]]")
	if end < len(prefix) {
		return "", text
	}
	id := strings.TrimSpace(s[len(prefix):end])
	rest := strings.TrimSpace(s[end+2:])
	if rest == "" || !b.history.hasMsgID(ch, id) {
		return "", text
	}
	return id, rest
}

// logTools returns the model's tool-call names for logging.
func logTools(calls []ToolCall) string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// setTyping emits the client-only "+typing" tag.
func (b *Bot) setTyping(c *girc.Client, ch, state string) {
	if !b.cfg.Bot.Typing || !c.IsConnected() || !c.HasCapability("message-tags") || !c.IsInChannel(ch) {
		return
	}
	tags := girc.Tags{}
	if err := tags.Set("+typing", state); err != nil {
		return
	}
	c.Send(&girc.Event{Command: "TAGMSG", Tags: tags, Params: []string{ch}})
}

// sendAnswer posts one answer line, tagging the first as a reply when replyTo is set.
func (b *Bot) sendAnswer(c *girc.Client, ch, line, replyTo string, first bool) {
	if first && replyTo != "" && c.HasCapability("message-tags") {
		tags := girc.Tags{}
		if err := tags.Set("+reply", replyTo); err == nil {
			c.Send(&girc.Event{Command: girc.PRIVMSG, Tags: tags, Params: []string{ch, line}})
			return
		}
	}
	c.Cmd.Message(ch, line)
}

// presenceSummary renders a bounded snapshot of the channel's tracked members.
func (b *Bot) presenceSummary(c *girc.Client, ch string) string {
	if !b.cfg.Bot.Presence {
		return ""
	}
	members := b.members.summary(ch, c.GetNick())
	if len(members) == 0 {
		return ""
	}
	total := len(members)
	const maxMembers = 40
	parts := make([]string, 0, min(total, maxMembers))
	for _, m := range members {
		if len(parts) >= maxMembers {
			break
		}
		entry := clean(m.nick)
		if m.account != "" {
			entry += " (account " + clean(m.account) + ")"
		}
		if m.away != "" {
			entry += " (away: " + logNote(m.away) + ")"
		}
		parts = append(parts, entry)
	}
	more := ""
	if total > maxMembers {
		more = fmt.Sprintf(", and %d more", total-maxMembers)
	}
	return fmt.Sprintf("%s has %d other member(s): %s%s", ch, total, strings.Join(parts, ", "), more)
}

// ownSummary lists the bot's recent messages so the model can target one to redact.
func (b *Bot) ownSummary(ch string) string {
	msgs := b.own.recent(ch, 5)
	if len(msgs) == 0 {
		return ""
	}
	var parts []string
	for _, m := range msgs {
		text := m.text
		if r := []rune(text); len(r) > 80 {
			text = string(r[:80]) + "…"
		}
		parts = append(parts, fmt.Sprintf("[msgid:%s] %s", m.id, text))
	}
	return strings.Join(parts, "\n")
}

// maybeReact runs a rate-limited, reaction-only evaluation of an ordinary
// message. It never produces chat text and never blocks a real answer.
func (b *Bot) maybeReact(ctx context.Context, c *girc.Client, ch string) {
	r := b.cfg.Bot.Reactions
	if !r.Enabled || !r.Spontaneous || b.busy.Load() {
		return
	}
	interval := time.Duration(r.MinIntervalSeconds) * time.Second
	b.mu.Lock()
	if time.Since(b.lastReact) < interval {
		b.mu.Unlock()
		return
	}
	b.lastReact = time.Now()
	b.mu.Unlock()
	if !b.reacting.CompareAndSwap(false, true) {
		return
	}
	snapshot := b.history.snapshot(ch)
	b.requests.Add(1)
	go func() {
		defer b.requests.Done()
		defer b.reacting.Store(false)
		reqCtx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.OpenAI.TimeoutSeconds)*time.Second)
		defer cancel()
		rep, err := b.ai.reply(reqCtx, snapshot, replyOptions{spontaneous: true, presence: b.presenceSummary(c, ch)})
		if err != nil {
			// A silent no-op is normal here; only real failures are worth a log.
			slog.Warn("reaction check failed", "error", err)
			return
		}
		if ctx.Err() != nil || !c.IsConnected() || !c.IsInChannel(ch) {
			return
		}
		sent := b.applyReactions(c, ch, rep.Calls)
		// Log-only: the model's private note is never sent to IRC, including when
		// it decided that no reaction was warranted.
		slog.Info("reaction check", "channel", ch, "reactions", sent, "note", logNote(rep.Text))
	}()
}

// acceptTags records incoming reactions (+draft/react / +draft/unreact) as context.
func (b *Bot) acceptTags(c *girc.Client, e girc.Event) {
	if !b.ready.Load() || e.Source == nil || len(e.Params) != 1 || strings.EqualFold(e.Source.Name, c.GetNick()) {
		return
	}
	if _, ok := e.Tags.Get("batch"); ok {
		return
	}
	// Recording a removal keeps the model from acting on a stale reaction.
	react, ok := e.Tags.Get("+draft/react")
	action := "reacted"
	if !ok {
		react, ok = e.Tags.Get("+draft/unreact")
		action = "removed reaction"
	}
	if !ok {
		return
	}
	react = clean(react)
	if len(react) > 32 {
		return
	}
	ch := b.allowedChannel(e.Params[0])
	if ch == "" {
		return
	}
	// Current clients use "+reply"; accept the older draft tag too.
	replyTo, ok := e.Tags.Get("+reply")
	if !ok {
		replyTo, _ = e.Tags.Get("+draft/reply")
	}
	account, _ := e.Tags.Get("account")
	b.history.add(ch, Message{Role: "user", Content: fmt.Sprintf("IRC nick=%s account=%s: %s %s to msgid=%s", e.Source.Name, account, action, react, replyTo)})
	slog.Info("reaction received", "channel", ch, "nick", e.Source.Name, "action", action, "emoji", react, "msgid", replyTo)
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
	cfg := girc.Config{Server: b.cfg.IRC.Server, Port: b.cfg.IRC.Port, Nick: b.cfg.IRC.Nick, User: b.cfg.IRC.User, Name: "Configurable IRC assistant", SSL: b.cfg.IRC.TLS, TLSConfig: tc, DisableSTS: true, ServerPass: b.cfg.IRC.Password, Version: "kat-irc " + version, HandleNickCollide: func(string) string { return "" }}
	if b.cfg.IRC.SASLUser != "" {
		cfg.SASL = &girc.SASLPlain{User: b.cfg.IRC.SASLUser, Pass: b.cfg.IRC.SASLPassword}
	}
	if b.cfg.Bot.Redaction {
		// echo-message is how we learn the msgids of our own messages.
		cfg.SupportedCaps = map[string][]string{"echo-message": nil, "draft/message-redaction": nil}
	}
	c := girc.New(cfg)
	var saslOK atomic.Bool
	var registered atomic.Bool
	var joined sync.Map
	updateReady := func() {
		n := 0
		joined.Range(func(_, _ any) bool { n++; return true })
		wasReady := b.ready.Load()
		nowReady := registered.Load() && n == len(b.cfg.IRC.Channels)
		b.ready.Store(nowReady)
		if nowReady && !wasReady && b.cfg.Bot.Images.Enabled && b.filehost(c) == "" {
			slog.Warn("images enabled but no upload host; set images.filehost or advertise soju.im/FILEHOST/draft/FILEHOST")
		}
	}
	started := time.Now()
	c.Handlers.Add("903", func(c *girc.Client, _ girc.Event) {
		saslOK.Store(true)
		// soju re-advertises sasl (CAP NEW) once the upstream connects. Dropping
		// it stops girc re-authenticating, which soju would forward upstream.
		c.Config.SASL = nil
	})
	c.Handlers.Add("005", func(_ *girc.Client, e girc.Event) {
		// girc needs ISUPPORT to end in "this server"; soju ends in "are supported".
		if v := filehostFromISupport(e); v != "" {
			b.filehostURL.Store(v)
			if b.cfg.Bot.Images.Enabled {
				slog.Info("image upload host advertised", "filehost", v)
			}
		}
	})
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
		slog.Info("connected", "server", b.cfg.IRC.Server,
			"message-tags", c.HasCapability("message-tags"),
			"account-tag", c.HasCapability("account-tag"),
			"echo-message", c.HasCapability("echo-message"),
			"message-redaction", c.HasCapability("draft/message-redaction"),
		)
		if b.cfg.Bot.Redaction && (!c.HasCapability("echo-message") || !c.HasCapability("draft/message-redaction")) {
			slog.Warn("redaction enabled but the server did not negotiate draft/message-redaction",
				"echo-message", c.HasCapability("echo-message"),
				"message-redaction", c.HasCapability("draft/message-redaction"))
		}
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
	c.Handlers.Add("TAGMSG", func(c *girc.Client, e girc.Event) { b.acceptTags(c, e) })
	for _, numeric := range []string{"433", "432", "471", "473", "474", "475", "476", "477", "489", "904", "905", "906"} {
		c.Handlers.Add(numeric, func(c *girc.Client, e girc.Event) {
			slog.Error("IRC registration/join failed", "numeric", e.Command)
			go c.Close()
		})
	}
	if b.cfg.Bot.Redaction {
		c.Handlers.Add(girc.ALL_EVENTS, func(c *girc.Client, e girc.Event) {
			if e.Command != girc.PRIVMSG || e.Source == nil || len(e.Params) < 1 || !strings.EqualFold(e.Source.Name, c.GetNick()) {
				return
			}
			id, _ := e.Tags.Get("msgid")
			if id == "" {
				return
			}
			if ch := b.allowedChannel(e.Params[0]); ch != "" {
				b.own.add(ch, id, clean(e.Last()))
			}
		})
	}
	if b.cfg.Bot.Presence {
		b.registerPresence(c)
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
