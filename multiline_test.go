package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lrstanley/girc"
)

func parseEvent(t *testing.T, s string) girc.Event {
	t.Helper()
	e := girc.ParseEvent(s)
	if e == nil {
		t.Fatal("invalid test event", s)
	}
	return *e
}
func testTransport(t *testing.T, attributes string) *messageTransport {
	t.Helper()
	tr := newMessageTransport(time.Now().Add(-time.Minute))
	tr.cap(parseEvent(t, ":server CAP * LS :message-tags batch draft/multiline="+attributes))
	tr.cap(parseEvent(t, ":server CAP Kat ACK :message-tags batch draft/multiline"))
	return tr
}
func TestMultilineCapabilities(t *testing.T) {
	for _, value := range []string{"", "max-lines=10", "max-bytes=0", "max-bytes=-1", "max-bytes=1", "max-bytes=no", "max-bytes=10,max-bytes=20", "max-bytes=100,max-lines=0"} {
		if got := parseMultilineLimits(value); got.bytes != 0 {
			t.Fatal("invalid capability accepted", value, got)
		}
	}
	tr := testTransport(t, "max-bytes=4096,max-lines=100,unknown=value")
	if got := tr.outgoingLimits(); got.bytes != 4096 || got.lines != 100 {
		t.Fatal(got)
	}
	tr.cap(parseEvent(t, ":server CAP * NEW :draft/multiline=max-bytes=2048,max-lines=20"))
	if tr.outgoingLimits().bytes != 2048 {
		t.Fatal("CAP NEW limits not refreshed")
	}
	tr.cap(parseEvent(t, ":server CAP * DEL :draft/multiline"))
	if tr.outgoingLimits().bytes != 0 {
		t.Fatal("CAP DEL did not disable sending")
	}
	tr.cap(parseEvent(t, ":server CAP * ACK :draft/multiline"))
	tr.fail(parseEvent(t, ":server FAIL BATCH MULTILINE_MAX_BYTES 2048 :limit exceeded"))
	if tr.outgoingLimits().bytes != 0 {
		t.Fatal("failed batch did not enable fallback")
	}
}
func TestMultilineSplitAndPack(t *testing.T) {
	text := "First é😀 paragraph with enough words to wrap correctly.\n\n  code();\nlast"
	fragments, err := splitLogical(text, 17)
	if err != nil {
		t.Fatal(err)
	}
	rebuild := func(fs []wireFragment) string {
		var s strings.Builder
		for i, f := range fs {
			if i > 0 && !f.concat {
				s.WriteByte('\n')
			}
			s.WriteString(f.text)
		}
		return s.String()
	}
	if got := rebuild(fragments); got != text {
		t.Fatalf("lost text/whitespace: %q", got)
	}
	for _, f := range fragments {
		if len(f.text) > 17 || !utf8.ValidString(f.text) {
			t.Fatal(f)
		}
	}
	batches := packMultiline(fragments, multilineLimits{bytes: 30, lines: 2})
	for _, b := range batches {
		if len(b) > 2 || len(rebuild(b)) > 30 || b[0].concat || strings.TrimSpace(rebuild(b)) == "" {
			t.Fatal("invalid batch", b)
		}
	}
	for _, b := range packMultiline([]wireFragment{{text: "a"}, {}, {text: "b"}}, multilineLimits{bytes: 10, lines: 1}) {
		if strings.TrimSpace(rebuild(b)) == "" {
			t.Fatal("blank-only batch")
		}
	}
}
func TestMultilineLiveAssemblyAndReplay(t *testing.T) {
	tr := testTransport(t, "max-bytes=4096,max-lines=100")
	now := time.Now()
	allowed := func(ch string) string {
		if ch == "#chat" {
			return ch
		}
		return ""
	}
	tr.batch(parseEvent(t, "@msgid=logical;account=alice :Alice!u@h BATCH +a draft/multiline #chat"), allowed, now)
	for _, s := range []string{"@batch=a :Alice!u@h PRIVMSG #chat :Kat: first ", "@batch=a;draft/multiline-concat :Alice!u@h PRIVMSG #chat :part", "@batch=a :Alice!u@h PRIVMSG #chat :", "@batch=a :Alice!u@h PRIVMSG #chat :  code();"} {
		tr.line(parseEvent(t, s), now)
	}
	e := tr.batch(parseEvent(t, ":Alice!u@h BATCH -a"), allowed, now)
	if e == nil || e.Last() != "Kat: first part\n\n  code();" {
		t.Fatal(e)
	}
	if id, _ := e.Tags.Get("msgid"); id != "logical" {
		t.Fatal("lost logical message id")
	}
	if account, _ := e.Tags.Get("account"); account != "alice" {
		t.Fatal("lost account")
	}
	if _, exists := e.Tags.Get("batch"); exists {
		t.Fatal("batch tag escaped assembly")
	}
	tr.batch(parseEvent(t, ":server BATCH +history chathistory #chat"), allowed, now)
	tr.batch(parseEvent(t, "@batch=history;account=alice :Alice!u@h BATCH +nested draft/multiline #chat"), allowed, now)
	tr.line(parseEvent(t, "@batch=nested :Alice!u@h PRIVMSG #chat :Kat: old message"), now)
	if e := tr.batch(parseEvent(t, "@batch=history :Alice!u@h BATCH -nested"), allowed, now); e != nil {
		t.Fatal("replay processed", e)
	}
}
func TestMultilineRejectsInvalidBatches(t *testing.T) {
	cases := [][]string{
		{"@batch=a :Mallory!u@h PRIVMSG #chat :Kat: spoof"},
		{"@batch=a :Alice!u@h PRIVMSG #other :Kat: wrong channel"},
		{"@batch=a;account=mallory :Alice!u@h PRIVMSG #chat :Kat: wrong account"},
		{"@batch=a;draft/multiline-concat :Alice!u@h PRIVMSG #chat :Kat: starts with concat"},
		{"@batch=a :Alice!u@h PRIVMSG #chat :Kat: valid", "@batch=a;draft/multiline-concat :Alice!u@h PRIVMSG #chat :"},
		{"@batch=a;time=2020-01-01T00:00:00Z :Alice!u@h PRIVMSG #chat :Kat: replay"},
		{"@batch=a :Alice!u@h NOTICE #chat :Kat: mixed command"},
		{"@batch=a :Alice!u@h PRIVMSG #chat :" + strings.Repeat("x", 100)},
	}
	for _, lines := range cases {
		tr := testTransport(t, "max-bytes=64,max-lines=2")
		now := time.Now()
		allowed := func(ch string) string { return ch }
		tr.batch(parseEvent(t, "@account=alice :Alice!u@h BATCH +a draft/multiline #chat"), allowed, now)
		for _, s := range lines {
			tr.line(parseEvent(t, s), now)
		}
		if got := tr.batch(parseEvent(t, ":Alice!u@h BATCH -a"), allowed, now); got != nil {
			t.Fatalf("accepted invalid batch %v: %v", lines, got)
		}
	}
	tr := testTransport(t, "max-bytes=64,max-lines=2")
	now := time.Now()
	allowed := func(ch string) string { return ch }
	tr.batch(parseEvent(t, ":Alice!u@h BATCH +a draft/multiline #chat"), allowed, now)
	tr.line(parseEvent(t, "@batch=a :Alice!u@h PRIVMSG #chat :Kat: incomplete"), now)
	if tr.batch(parseEvent(t, ":Alice!u@h BATCH -a"), allowed, now.Add(3*time.Minute)) != nil {
		t.Fatal("expired batch processed")
	}
}
func TestPrepareAnswerKeepsParagraphsSourcesAndBudget(t *testing.T) {
	sources := []webSource{{URL: "https://example.org/source"}}
	text, err := prepareAnswer(strings.Repeat("é😀 long answer. ", 200)+"\x01", sources, []string{"https://example.org/image"}, 512)
	if err != nil || len(text) > 512 || !utf8.ValidString(text) || !strings.HasSuffix(text, "[1] https://example.org/source\nhttps://example.org/image") {
		t.Fatal(text, err)
	}
	got, err := prepareAnswer("one\r\n\r\n  code();\x00", nil, nil, 1000)
	if err != nil || got != "one\n\n  code();" {
		t.Fatal(got, err)
	}
	var c Config
	if c.replyByteBudget() != 8192 {
		t.Fatal("missing automatic default budget")
	}
	c.Bot.MaxReplyLines = 16
	if c.replyByteBudget() != 5600 {
		t.Fatal("legacy response budget lost")
	}
}

type multilineIRC struct {
	conn   net.Conn
	scan   *bufio.Scanner
	cancel context.CancelFunc
	done   chan error
}

func startMultilineIRC(t *testing.T, b *Bot, caps string) *multilineIRC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.IRC.Server = "127.0.0.1"
	b.cfg.IRC.Port = ln.Addr().(*net.TCPAddr).Port
	b.cfg.IRC.TLS = false
	ctx, cancel := context.WithCancel(context.Background())
	f := &multilineIRC{cancel: cancel, done: make(chan error, 1)}
	go func() { f.done <- b.run(ctx) }()
	f.conn, err = ln.Accept()
	ln.Close()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	f.conn.SetDeadline(time.Now().Add(30 * time.Second))
	f.scan = bufio.NewScanner(f.conn)
	t.Cleanup(func() {
		cancel()
		f.conn.Close()
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Error("IRC shutdown stuck")
		}
	})
	f.until(t, "CAP LS")
	f.send(t, ":server CAP * LS :"+caps)
	req := f.until(t, "CAP REQ :")
	f.send(t, ":server CAP Kat ACK :"+strings.TrimPrefix(req, "CAP REQ :"))
	f.until(t, "CAP END")
	f.send(t, ":server 001 Kat :Welcome")
	f.send(t, ":server 005 Kat CHANTYPES=# NICKLEN=32 USERLEN=20 HOSTLEN=63 :are supported by this server")
	f.send(t, ":server 376 Kat :End MOTD")
	f.until(t, "JOIN #chat")
	f.send(t, ":Kat!kat@host.example JOIN #chat")
	f.send(t, ":server 353 Kat = #chat :Kat Alice")
	f.send(t, ":server 366 Kat #chat :End NAMES")
	f.send(t, "PING :ready")
	f.until(t, "PONG")
	return f
}
func (f *multilineIRC) send(t *testing.T, s string) {
	t.Helper()
	if _, err := fmt.Fprint(f.conn, s+"\r\n"); err != nil {
		t.Fatal(err)
	}
}
func (f *multilineIRC) until(t *testing.T, prefix string) string {
	t.Helper()
	for f.scan.Scan() {
		s := f.scan.Text()
		if strings.HasPrefix(s, prefix) {
			return s
		}
	}
	t.Fatalf("missing %q: %v", prefix, f.scan.Err())
	return ""
}
func TestMultilineTCPNegotiationAndOutbound(t *testing.T) {
	answer := strings.Repeat("é😀 words ", 50) + "\n\n  code();"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sse(answer)) }))
	defer api.Close()
	c, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c.OpenAI.Auth = "api_key"
	c.OpenAI.APIKey = "test"
	c.Bot.Reactions.Enabled = false
	c.Bot.Presence = false
	c.Bot.Typing = false
	c.Bot.ReplyThreading = "always"
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: &AI{cfg: c, http: api.Client(), endpoint: api.URL}, history: h}
	f := startMultilineIRC(t, b, "message-tags batch server-time account-tag draft/multiline=max-bytes=4096,max-lines=100")
	f.send(t, "@account=alice;msgid=question :Alice!u@h PRIVMSG #chat :Kat: explain")
	opening := parseEvent(t, f.until(t, "@+reply=question BATCH +"))
	id := strings.TrimPrefix(opening.Params[0], "+")
	var body strings.Builder
	count := 0
	hasConcat := false
	for f.scan.Scan() {
		raw := f.scan.Text()
		e := parseEvent(t, raw)
		if e.Command == "BATCH" && e.Params[0] == "-"+id {
			break
		}
		if e.Command != girc.PRIVMSG {
			continue
		}
		batch, _ := e.Tags.Get("batch")
		if batch != id {
			t.Fatal("wrong batch id", raw)
		}
		if _, bad := e.Tags.Get("+reply"); bad {
			t.Fatal("reply metadata sent on component instead of opening")
		}
		_, concat := e.Tags.Get(concatTag)
		hasConcat = hasConcat || concat
		if count > 0 && !concat {
			body.WriteByte('\n')
		}
		body.WriteString(e.Last())
		count++
		if len(raw) > 510 || !utf8.ValidString(raw) {
			t.Fatal("invalid wire component", len(raw))
		}
	}
	if body.String() != answer || !hasConcat || count < 3 {
		t.Fatalf("multiline lost content: %q", body.String())
	}
}
func TestMultilineTCPInboundIsOneObservationAndInference(t *testing.T) {
	var calls atomic.Int32
	inputs := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		data, _ := json.Marshal(body["input"])
		calls.Add(1)
		inputs <- string(data)
		fmt.Fprint(w, sse("ok"))
	}))
	defer api.Close()
	c, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c.OpenAI.Auth = "api_key"
	c.OpenAI.APIKey = "test"
	c.Bot.Reactions.Enabled = false
	c.Bot.Presence = false
	c.Bot.Typing = false
	c.Bot.Redaction = true
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: &AI{cfg: c, http: api.Client(), endpoint: api.URL}, history: h, own: newOwnMessages(50)}
	f := startMultilineIRC(t, b, "message-tags batch account-tag server-time echo-message draft/message-redaction draft/multiline=max-bytes=4096,max-lines=100")
	f.send(t, ":server BATCH +history chathistory #chat")
	f.send(t, "@batch=history;account=alice :Alice!u@h BATCH +old draft/multiline #chat")
	f.send(t, "@batch=old :Alice!u@h PRIVMSG #chat :Kat: replay")
	f.send(t, "@batch=history :Alice!u@h BATCH -old")
	f.send(t, ":server BATCH -history")
	f.send(t, "@account=alice :Alice!u@h BATCH +malformed draft/multiline #chat")
	f.send(t, "@batch=malformed :Alice!u@h PRIVMSG #chat :Kat: invalid nested batch")
	f.send(t, "@batch=malformed :Alice!u@h BATCH +child draft/multiline #chat")
	f.send(t, "@batch=malformed :Alice!u@h BATCH -child")
	f.send(t, ":Alice!u@h BATCH -malformed")
	f.send(t, "@account=alice;msgid=logical :Alice!u@h BATCH +live draft/multiline #chat")
	f.send(t, "@batch=live :Alice!u@h PRIVMSG #chat :Kat: first ")
	f.send(t, "@batch=live;draft/multiline-concat :Alice!u@h PRIVMSG #chat :part")
	f.send(t, "@batch=live :Alice!u@h PRIVMSG #chat :second line")
	f.send(t, ":Alice!u@h BATCH -live")
	select {
	case input := <-inputs:
		if !strings.Contains(input, `first part\nsecond line`) || strings.Contains(input, "replay") {
			t.Fatal(input)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inference for completed live message")
	}
	f.until(t, "PRIVMSG #chat ")
	f.send(t, "@msgid=own-logical :Kat!kat@host.example BATCH +echo draft/multiline #chat")
	f.send(t, "@batch=echo :Kat!kat@host.example PRIVMSG #chat :my first line")
	f.send(t, "@batch=echo :Kat!kat@host.example PRIVMSG #chat :my second line")
	f.send(t, ":Kat!kat@host.example BATCH -echo")
	f.send(t, "PING :barrier")
	f.until(t, "PONG")
	if calls.Load() != 1 {
		t.Fatal("multiple calls for one logical message", calls.Load())
	}
	own := b.own.recent("#chat", 10)
	if len(own) != 1 || own[0].id != "own-logical" || own[0].text != "my first line\nmy second line" {
		t.Fatal("echo fragments were not grouped", own)
	}
}

func TestMultilineReceiveBuffersStayBounded(t *testing.T) {
	tr := testTransport(t, "max-bytes=65536,max-lines=512")
	now := time.Now()
	allowed := func(ch string) string { return ch }
	for i := 0; i < 32; i++ {
		tr.batch(parseEvent(t, fmt.Sprintf(":Alice!u@h BATCH +b%d draft/multiline #chat", i)), allowed, now)
	}
	if len(tr.batches) > maxIncomingBatches {
		t.Fatal("batch count exceeded safety ceiling")
	}
	for j := 0; j < 50; j++ {
		for i := 0; i < 16; i++ {
			tr.line(parseEvent(t, fmt.Sprintf("@batch=b%d :Alice!u@h PRIVMSG #chat :%s", i, strings.Repeat("x", 400))), now)
			total := 0
			for _, row := range tr.batches {
				total += row.text.Len()
			}
			if total > maxBufferedBytes {
				t.Fatal("receive memory exceeded safety ceiling", total)
			}
		}
	}
	tr.off = true
	if tr.outgoingLimits().bytes != 0 {
		t.Fatal("off mode did not force fallback")
	}
}

func TestMultilineSourceURLRemainsWholeOnFallback(t *testing.T) {
	url := "https://example.org/" + strings.Repeat("x", 60)
	text := "[1] " + url
	fs, err := splitLogical(text, len(url))
	if err != nil || len(fs) != 2 || fs[1].text != url || !fs[1].concat {
		t.Fatal("citation URL split inside word", fs, err)
	}
}
