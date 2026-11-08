package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lrstanley/girc"
)

const multilineCap = "draft/multiline"
const concatTag = "draft/multiline-concat"
const maxLogicalBytes = 65536
const maxIncomingBatches = 16
const maxBufferedBytes = 262144
const maxBatchAge = 2 * time.Minute

type multilineLimits struct{ bytes, lines int }
type incomingMultiline struct {
	event   girc.Event
	source  string
	text    strings.Builder
	lines   int
	invalid bool
	began   time.Time
}

// Each connection gets its own transport state, including buffers and CAP values.
// Never carry partial batches or advertised limits across reconnects.
type messageTransport struct {
	mu      sync.Mutex
	limits  multilineLimits
	enabled map[string]bool
	batches map[string]*incomingMultiline
	failed  bool
	off     bool
	started time.Time
	serial  atomic.Uint64
}

func newMessageTransport(started time.Time) *messageTransport {
	return &messageTransport{enabled: map[string]bool{}, batches: map[string]*incomingMultiline{}, started: started}
}
func parseMultilineLimits(value string) multilineLimits {
	var out multilineLimits
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			if part == "max-bytes" || part == "max-lines" {
				return multilineLimits{}
			}
			continue
		}
		if kv[0] != "max-bytes" && kv[0] != "max-lines" {
			continue
		}
		if seen[kv[0]] {
			return multilineLimits{}
		}
		seen[kv[0]] = true
		n, err := strconv.Atoi(kv[1])
		if err != nil || n < 1 {
			return multilineLimits{}
		}
		if kv[0] == "max-bytes" {
			out.bytes = n
		} else {
			out.lines = n
		}
	}
	if out.bytes < 4 {
		return multilineLimits{}
	}
	if out.bytes > maxLogicalBytes {
		out.bytes = maxLogicalBytes
	}
	if out.lines == 0 || out.lines > 512 {
		out.lines = 512
	}
	return out
}
func (t *messageTransport) cap(e girc.Event) {
	if len(e.Params) < 3 {
		return
	}
	sub := e.Params[1]
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, token := range strings.Fields(e.Last()) {
		kv := strings.SplitN(token, "=", 2)
		name := strings.TrimPrefix(kv[0], "-")
		switch sub {
		case "LS", "NEW":
			if name == multilineCap {
				t.limits = multilineLimits{}
				if len(kv) == 2 {
					t.limits = parseMultilineLimits(kv[1])
				}
				t.failed = false
			}
		case "ACK":
			t.enabled[name] = !strings.HasPrefix(token, "-")
			if name == multilineCap && len(kv) == 2 {
				t.limits = parseMultilineLimits(kv[1])
			}
		case "DEL":
			delete(t.enabled, name)
			if name == multilineCap || name == "batch" {
				t.batches = map[string]*incomingMultiline{}
			}
		}
	}
}
func (t *messageTransport) limitsLocked() multilineLimits {
	if t.off || !t.enabled[multilineCap] || !t.enabled["batch"] || !t.enabled["message-tags"] {
		return multilineLimits{}
	}
	return t.limits
}
func (t *messageTransport) outgoingLimits() multilineLimits {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed {
		return multilineLimits{}
	}
	return t.limitsLocked()
}
func (t *messageTransport) fail(e girc.Event) {
	if len(e.Params) > 1 && e.Params[0] == "BATCH" && strings.HasPrefix(e.Params[1], "MULTILINE_") {
		t.mu.Lock()
		t.failed = true
		t.mu.Unlock()
		slog.Warn("server rejected multiline; using plain messages for subsequent replies", "code", e.Params[1])
	}
}
func oldEvent(e girc.Event, started time.Time) bool {
	if ts, ok := e.Tags.Get("time"); ok {
		if at, err := time.Parse(time.RFC3339Nano, ts); err == nil && at.Before(started) {
			return true
		}
	}
	return false
}
func validBatchID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func (t *messageTransport) expireLocked(now time.Time) {
	for id, b := range t.batches {
		if now.Sub(b.began) > maxBatchAge {
			delete(t.batches, id)
		}
	}
}

// Only negotiated, top-level live multiline batches are assembled. Nested batches
// (including multiline inside CHATHISTORY), unknown batch references and replay
// are deliberately ignored. No partial message reaches the model or memory.
func (t *messageTransport) batch(e girc.Event, allowed func(string) string, now time.Time) *girc.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(now)
	if len(e.Params) == 0 {
		return nil
	}
	ref := e.Params[0]
	if len(ref) < 2 || !validBatchID(ref[1:]) {
		return nil
	}
	id := ref[1:]
	if ref[0] == '+' {
		if _, exists := t.batches[id]; exists {
			t.batches[id].invalid = true
			return nil
		}
		if len(e.Params) != 3 || e.Params[1] != multilineCap || e.Source == nil || t.limitsLocked().bytes == 0 || len(t.batches) >= maxIncomingBatches {
			return nil
		}
		if _, nested := e.Tags.Get("batch"); nested {
			return nil
		}
		ch := allowed(e.Params[2])
		if ch == "" || oldEvent(e, t.started) {
			return nil
		}
		event := *e.Copy()
		event.Command = girc.PRIVMSG
		event.Params = []string{ch, ""}
		delete(event.Tags, "batch")
		delete(event.Tags, concatTag)
		t.batches[id] = &incomingMultiline{event: event, source: e.Source.String(), began: now}
		return nil
	}
	if ref[0] != '-' {
		return nil
	}
	b := t.batches[id]
	delete(t.batches, id)
	if len(e.Params) != 1 || b == nil || b.invalid || b.lines == 0 || t.limitsLocked().bytes == 0 {
		return nil
	}
	if _, nested := e.Tags.Get("batch"); nested {
		return nil
	}
	if e.Source != nil && e.Source.String() != b.source {
		return nil
	}
	if strings.TrimSpace(b.text.String()) == "" {
		return nil
	}
	b.event.Params[1] = b.text.String()
	return &b.event
}
func (t *messageTransport) line(e girc.Event, now time.Time) {
	id, ok := e.Tags.Get("batch")
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(now)
	b := t.batches[id]
	if b == nil || b.invalid {
		return
	}
	limits := t.limitsLocked()
	if limits.bytes == 0 || e.Command != girc.PRIVMSG || e.Source == nil || e.Source.String() != b.source || len(e.Params) != 2 || girc.ToRFC1459(e.Params[0]) != b.event.Params[0] || oldEvent(e, t.started) {
		b.invalid = true
		b.text.Reset()
		return
	}
	concat := false
	if _, ok := e.Tags.Get(concatTag); ok {
		concat = true
	}
	if concat && (b.lines == 0 || e.Last() == "") {
		b.invalid = true
		b.text.Reset()
		return
	}
	// Some servers repeat metadata on component lines. Never permit a change in
	// account within a logical message. Metadata on the opening BATCH wins.
	if account, ok := e.Tags.Get("account"); ok {
		if prior, present := b.event.Tags.Get("account"); present && prior != account {
			b.invalid = true
			b.text.Reset()
			return
		}
	}
	if b.lines == 0 {
		if b.event.Tags == nil {
			b.event.Tags = girc.Tags{}
		}
		for _, key := range []string{"account", "time", "msgid", "+reply"} {
			if _, present := b.event.Tags.Get(key); !present {
				if value, ok := e.Tags.Get(key); ok {
					b.event.Tags.Set(key, value)
				}
			}
		}
	}
	extra := len(e.Last())
	if b.lines > 0 && !concat {
		extra++
	}
	buffered := 0
	for _, row := range t.batches {
		buffered += row.text.Len()
	}
	if b.lines+1 > limits.lines || b.text.Len()+extra > limits.bytes || buffered+extra > maxBufferedBytes || !utf8.ValidString(e.Last()) {
		b.invalid = true
		b.text.Reset()
		return
	}
	if b.lines > 0 && !concat {
		b.text.WriteByte('\n')
	}
	b.text.WriteString(e.Last())
	b.lines++
}

func logicalText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
func utf8Prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
func (c Config) replyByteBudget() int {
	budget := c.Bot.MaxReplyBytes
	if budget == 0 {
		budget = 8192
	}
	if c.Bot.MaxReplyLines > 0 && c.Bot.MaxReplyLines*350 < budget {
		budget = c.Bot.MaxReplyLines * 350
	}
	return budget
}

// The complete answer retains paragraphs. URLs are reserved before shortening the
// prose, so citation attribution and generated image links cannot be truncated.
func prepareAnswer(text string, sources []webSource, images []string, budget int) (string, error) {
	text = strings.TrimSpace(logicalText(text))
	var suffix []string
	for i, s := range sources {
		suffix = append(suffix, fmt.Sprintf("[%d] %s", i+1, s.URL))
	}
	for _, s := range images {
		if strings.IndexFunc(s, unicode.IsSpace) >= 0 || logicalText(s) != s {
			return "", fmt.Errorf("invalid image URL")
		}
		suffix = append(suffix, s)
	}
	tail := strings.Join(suffix, "\n")
	room := budget - len(tail)
	if text != "" && tail != "" {
		room -= 2
	}
	if room < 0 || (text != "" && room < 8) {
		return "", fmt.Errorf("answer byte budget cannot fit text and sources")
	}
	if len(text) > room {
		text = strings.TrimRight(utf8Prefix(text, room-len(" …")), " \n\t") + " …"
	}
	if text != "" && tail != "" {
		return text + "\n\n" + tail, nil
	}
	if text != "" {
		return text, nil
	}
	return tail, nil
}

// Stay below both the server's IRC line allowance and girc's own splitting limit.
// This prevents the library from re-splitting a multiline component invisibly.
func wirePayloadBytes(c *girc.Client, target string) (int, error) {
	host := c.GetHost()
	if host == "" {
		host = strings.Repeat("h", 63)
	}
	prefix := len(c.GetNick()) + len(c.GetIdent()) + len(host) + 4 // :nick!ident@host + space
	length := 512
	if n, ok := c.GetServerOptionInt("LINELEN"); ok && n >= 512 && n <= 8192 {
		length = n
	}
	overhead := len("PRIVMSG " + target + " :")
	n := length - 2 - prefix - overhead - 10
	if lib := c.MaxEventLength() - overhead - 1; lib < n {
		n = lib
	}
	if n < 4 {
		return 0, fmt.Errorf("insufficient IRC line space")
	}
	return n, nil
}

type wireFragment struct {
	text   string
	concat bool
}

func splitLogical(text string, maxBytes int) ([]wireFragment, error) {
	var fragments []wireFragment
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			fragments = append(fragments, wireFragment{})
			continue
		}
		continuation := false
		for len(line) > 0 {
			piece := utf8Prefix(line, maxBytes)
			if piece == "" {
				return nil, fmt.Errorf("server byte limit cannot fit a UTF-8 character")
			}
			if len(piece) < len(line) {
				if space := strings.LastIndexAny(piece, " \t"); space >= 0 {
					piece = piece[:space+1]
				}
			}
			fragments = append(fragments, wireFragment{text: piece, concat: continuation})
			continuation = true
			line = line[len(piece):]
			if len(fragments) > 2048 {
				return nil, fmt.Errorf("too many IRC fragments")
			}
		}
	}
	return fragments, nil
}
func packMultiline(fragments []wireFragment, limits multilineLimits) [][]wireFragment {
	var batches [][]wireFragment
	var current []wireFragment
	size := 0
	flush := func() {
		for _, f := range current {
			if strings.TrimSpace(f.text) != "" {
				batches = append(batches, current)
				return
			}
		}
	}
	for _, f := range fragments {
		extra := len(f.text)
		if len(current) > 0 && !f.concat {
			extra++
		}
		if len(current) > 0 && (len(current) >= limits.lines || size+extra > limits.bytes) {
			flush()
			current = nil
			size = 0
		}
		if len(current) == 0 {
			f.concat = false
			extra = len(f.text)
		}
		current = append(current, f)
		size += extra
	}
	if len(current) > 0 {
		flush()
	}
	return batches
}
func (b *Bot) sendLogicalAnswer(ctx context.Context, c *girc.Client, t *messageTransport, ch, text, replyTo string) error {
	payload, err := wirePayloadBytes(c, ch)
	if err != nil {
		return err
	}
	// Keep URLs whole for clients receiving the server's plain-message fallback.
	for _, word := range strings.Fields(text) {
		if (strings.HasPrefix(word, "https://") || strings.HasPrefix(word, "http://")) && len(word) > payload {
			return fmt.Errorf("URL does not fit an IRC component")
		}
	}
	limits := t.outgoingLimits()
	// Pathologically small batch limits are better served by normal IRC messages.
	if limits.bytes < 64 || payload < 64 {
		limits = multilineLimits{}
	}
	componentBytes := payload
	if limits.bytes > 0 && limits.bytes < componentBytes {
		componentBytes = limits.bytes
	}
	fragments, err := splitLogical(text, componentBytes)
	if err != nil {
		return err
	}
	sent := 0
	send := func(event *girc.Event) error {
		if sent > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(800 * time.Millisecond):
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !c.IsConnected() || !c.IsInChannel(ch) {
			return fmt.Errorf("IRC channel disconnected")
		}
		c.Send(event)
		sent++
		return nil
	}
	replyTags := func() girc.Tags {
		tags := girc.Tags{}
		if replyTo != "" && c.HasCapability("message-tags") {
			tags.Set("+reply", replyTo)
		}
		return tags
	}
	if limits.bytes == 0 || len(fragments) == 1 {
		first := true
		for _, f := range fragments {
			if strings.TrimSpace(f.text) == "" {
				continue
			}
			tags := girc.Tags{}
			if first {
				tags = replyTags()
			}
			first = false
			if err = send(&girc.Event{Command: girc.PRIVMSG, Tags: tags, Params: []string{ch, f.text}}); err != nil {
				return err
			}
		}
		return nil
	}
	for i, batch := range packMultiline(fragments, limits) {
		id := fmt.Sprintf("kat-%d", t.serial.Add(1))
		tags := girc.Tags{}
		if i == 0 {
			tags = replyTags()
		}
		if err = send(&girc.Event{Command: "BATCH", Tags: tags, Params: []string{"+" + id, multilineCap, ch}}); err != nil {
			return err
		}
		for _, f := range batch {
			tags = girc.Tags{}
			tags.Set("batch", id)
			if f.concat {
				tags.Set(concatTag, "")
			}
			if err = send(&girc.Event{Command: girc.PRIVMSG, Tags: tags, Params: []string{ch, f.text}}); err != nil {
				return err
			}
		}
		if err = send(&girc.Event{Command: "BATCH", Params: []string{"-" + id}}); err != nil {
			return err
		}
	}
	return nil
}
