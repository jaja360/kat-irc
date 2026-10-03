package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lrstanley/girc"
)

func TestTrigger(t *testing.T) {
	for _, s := range []string{"Kat: hello", "Kat:hello", "kat: hey", "@Kat hello", "@KAT\tyes", "@Kat", "Kat:"} {
		if !triggered(s, "Kat") {
			t.Errorf("should trigger: %q", s)
		}
	}
	for _, s := range []string{"hello Kat: hello", " @Kat hello", "@Katastrophe hello", "Kat hello", "@Kat: hello", "", "Kat"} {
		if triggered(s, "Kat") {
			t.Errorf("must not trigger: %q", s)
		}
	}
}
func TestReplyLines(t *testing.T) {
	in := strings.Repeat("Unicode é 😏 ", 200) + "\r\nPRIVMSG #else :injection\x01"
	lines := replyLines(in, 6)
	if len(lines) != 6 {
		t.Fatal(len(lines))
	}
	for _, l := range lines {
		if !utf8.ValidString(l) || len(l) > 354 || strings.ContainsAny(l, "\r\n\x00\x01") {
			t.Fatalf("unsafe line %q", l)
		}
	}
}
func TestHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	h, e := newHistory(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	h.add("#a", Message{Role: "user", Content: "one", MsgID: "m1"})
	h.add("#a", Message{Role: "assistant", Content: "two"})
	h.add("#a", Message{Role: "user", Content: "three", MsgID: "m3"})
	h.add("#b", Message{Role: "user", Content: "private"})
	if e = h.save(); e != nil {
		t.Fatal(e)
	}
	h, e = newHistory(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	if got := h.snapshot("#a"); len(got) != 2 || got[0].Content != "two" {
		t.Fatal(got)
	}
	if !h.hasMsgID("#a", "m3") || h.hasMsgID("#a", "m1") || h.hasMsgID("#b", "m3") || h.hasMsgID("#a", "") {
		t.Fatal("msgid lookup must be per channel and limited to kept messages")
	}
	if stat, e := os.Stat(path); e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("history permissions")
	}
}
func sse(text string) string {
	b, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
	return "data: " + string(b) + "\n\ndata: {\"type\":\"response.completed\"}\n\n"
}

// sseToolCall mimics a Responses API stream that emits text plus a completed
// function_call item, as observed in the real API (item id lives under item.id).
func sseToolCall(text, name, args, callID string) string {
	txt, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
	part, _ := json.Marshal(map[string]string{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": args[:1]})
	item, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]string{
		"type": "function_call", "id": "fc_1", "call_id": callID, "name": name, "arguments": args,
	}})
	return "data: " + string(txt) + "\n\ndata: " + string(part) + "\n\ndata: " + string(item) + "\n\ndata: {\"type\":\"response.completed\"}\n\n"
}
func TestSSE(t *testing.T) {
	out, e := consumeSSE(strings.NewReader(sse("Hello 😏")))
	if e != nil || out.Text != "Hello 😏" || len(out.Calls) != 0 {
		t.Fatal(out, e)
	}
	calls, e := consumeSSE(strings.NewReader(sseToolCall("Hi", reactToolName, `{"emoji":"😏","msgid":"abc"}`, "call_1")))
	if e != nil || calls.Text != "Hi" || len(calls.Calls) != 1 {
		t.Fatal(calls, e)
	}
	got := calls.Calls[0]
	if got.Name != reactToolName || got.CallID != "call_1" || got.Arguments != `{"emoji":"😏","msgid":"abc"}` {
		t.Fatal(got)
	}
	// A call-only stream is valid: the bot reacts without sending text.
	only, e := consumeSSE(strings.NewReader(sseToolCall("", reactToolName, `{"emoji":"🔥","msgid":"abc"}`, "call_2")))
	if e != nil || only.Text != "" || len(only.Calls) != 1 {
		t.Fatal(only, e)
	}
	// An empty completed stream is not a transport error; reply() decides
	// whether an empty answer is acceptable (spontaneous turns are).
	empty, e := consumeSSE(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
	if e != nil || empty.Text != "" || len(empty.Calls) != 0 {
		t.Fatal(empty, e)
	}
	for _, in := range []string{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "data: {\"type\":\"response.failed\"}\n\n", "data: {\"type\":\"response.incomplete\"}\n\n", "data: bad\n\n"} {
		if _, e := consumeSSE(strings.NewReader(in)); e == nil {
			t.Fatal("accepted incomplete stream", in)
		}
	}
}
func TestOpenAIRequest(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	for _, mode := range []string{"api_key", "chatgpt"} {
		for _, reactions := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reactions=%v", mode, reactions), func(t *testing.T) {
				var wantTools bool
				var instructions string
				var input []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
						t.Error("incorrect endpoint/auth")
					}
					var body map[string]any
					if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
						t.Error(e)
					}
					if body["store"] != false || body["stream"] != true {
						t.Error(body)
					}
					if _, ok := body["tools"]; ok != wantTools {
						t.Error("wrong tools payload for reaction setting")
					}
					instructions, _ = body["instructions"].(string)
					input, _ = json.Marshal(body["input"])
					_, hasMax := body["max_output_tokens"]
					if hasMax != (mode == "api_key") {
						t.Error("wrong max_output_tokens for auth mode")
					}
					fmt.Fprint(w, sseToolCall("Test", reactToolName, `{"emoji":"😏","msgid":"resp1"}`, "call_1"))
				}))
				defer server.Close()
				var c Config
				c.OpenAI.Auth = mode
				c.OpenAI.APIKey = "test-key"
				c.OpenAI.CredentialsFile = filepath.Join(t.TempDir(), "oauth.json")
				if e := atomicJSON(c.OpenAI.CredentialsFile, &Credentials{ClientID: "issued", Subject: "test", Scope: planScope, AccessToken: "test-key", ExpiresAt: time.Now().Add(time.Hour)}); e != nil {
					t.Fatal(e)
				}
				c.OpenAI.Model = "test"
				c.OpenAI.MaxOutputTokens = 100
				c.Bot.Persona = "test"
				c.Bot.Reactions.Enabled = reactions
				c.Bot.Reactions.MaxPerReply = 2
				wantTools = reactions
				a := &AI{cfg: c, http: server.Client(), endpoint: server.URL}
				msgs := []Message{{Role: "user", Content: "hello", MsgID: "req1"}}
				out, e := a.reply(context.Background(), msgs, replyOptions{})
				if e != nil || out.Text != "Test" || len(out.Calls) != 1 {
					t.Fatal(out, e)
				}
				if !strings.Contains(string(input), "[msgid:req1] hello") {
					t.Error("msgid not exposed to the model", string(input))
				}
				if reactions != strings.Contains(instructions, reactToolName) {
					t.Error("instructions must advertise the react tool when enabled", instructions)
				}
			})
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRefreshSerialized(t *testing.T) {
	old := authHTTP
	defer func() { authHTTP = old }()
	var calls atomic.Int32
	authHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if e := r.ParseForm(); e != nil {
			t.Error(e)
		}
		if r.Form.Get("client_id") != "issued" || r.Form.Get("refresh_token") != "old" {
			t.Error("wrong grant")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`)), Header: make(http.Header)}, nil
	})}
	path := filepath.Join(t.TempDir(), "oauth.json")
	c := &Credentials{ClientID: "issued", Subject: "user", Scope: planScope, AccessToken: "expired", RefreshToken: "old"}
	if e := atomicJSON(path, c); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, e := oauthToken(context.Background(), path)
			if e != nil || tok != "new" {
				t.Error(tok, e)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent refresh", calls.Load())
	}
	saved, e := readCredentials(path)
	if e != nil || saved.RefreshToken != "rotated" {
		t.Fatal(saved, e)
	}
}
func TestCredentialLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.json")
	unlock, e := lockFile(path)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	if unlock2, e := lockFile(path); e == nil {
		unlock2()
		t.Fatal("concurrent credential writer permitted")
	}
}
func TestIRCIntegration(t *testing.T) {
	// Actual TCP IRC negotiation + HTTP SSE, no external service or paid requests.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	apiStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(apiStarted)
		<-release
		fmt.Fprint(w, sse("The test is alive 😏"))
	}))
	defer func() { once.Do(func() { close(release) }); api.Close() }()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.AllowedAccounts = []string{"alice"}
	c.Bot.Reactions.Enabled = false // this test asserts exactly one API call
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(prefix string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		t.Fatalf("missing %s: %v", prefix, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	// Barrier: handlers are ordered before this PONG.
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	send("@account=alice :Alice!u@h PRIVMSG #chat :Hello Kat: nothing")
	send("@account=other :Other!u@h PRIVMSG #chat :Kat: unauthorized")
	send("@account=alice;batch=replay :Alice!u@h PRIVMSG #chat :Kat: replay")
	send("@account=alice;time=2020-01-01T00:00:00.000Z :Alice!u@h PRIVMSG #chat :Kat: old")
	send("@account=alice :Alice!u@h PRIVMSG #chat :@Kat hello")
	select {
	case <-apiStarted:
	case <-time.After(8 * time.Second):
		t.Fatal("no API call")
	}
	send("PING :during-inference")
	until("PONG")
	once.Do(func() { close(release) })
	got := until("PRIVMSG #chat :")
	if !strings.Contains(got, "The test is alive") {
		t.Fatal(got)
	}
	if calls.Load() != 1 {
		t.Fatal("wrong number of API calls", calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestLogNote(t *testing.T) {
	got := logNote("line1\nline2\x01 " + strings.Repeat("é", 500))
	if strings.ContainsAny(got, "\n\x01") {
		t.Fatal(got)
	}
	if n := utf8.RuneCountInString(got); n > 201 {
		t.Fatal("unbounded log note", n)
	}
}

func TestSpontaneousThrottle(t *testing.T) {
	// Ordinary messages must not accumulate inference: at most one reaction-only
	// evaluation per interval, and no IRC output when the model stays silent.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	var calls atomic.Int32
	checked := make(chan struct{}, 4)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if s, _ := body["instructions"].(string); !strings.Contains(s, "silent reaction check") {
			t.Error("spontaneous turn must use the silent instructions")
		}
		fmt.Fprint(w, sse("Nothing here deserves a reaction."))
		checked <- struct{}{}
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.Reactions.Enabled = true
	c.Bot.Reactions.Spontaneous = true
	c.Bot.Reactions.MinIntervalSeconds = 600
	c.Bot.Reactions.MaxPerReply = 2
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	for _, m := range []string{"first", "second", "third"} {
		send("@account=alice;msgid=" + m + " :Alice!u@h PRIVMSG #chat :" + m)
	}
	select {
	case <-checked:
	case <-time.After(8 * time.Second):
		t.Fatal("no spontaneous check")
	}
	send("PING :quiet")
	if got := until("PONG"); !strings.HasPrefix(got, "PONG") {
		t.Fatal(got)
	}
	// A silent decision must produce no PRIVMSG and no TAGMSG.
	conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	if scan.Scan() {
		t.Fatalf("unexpected IRC output: %q", scan.Text())
	}
	if calls.Load() != 1 {
		t.Fatal("throttle failed", calls.Load())
	}
	if got := b.history.snapshot("#chat"); len(got) != 3 {
		t.Fatal("throttled messages must stay as context", len(got))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestIRCReactions(t *testing.T) {
	// The bot must see IRCv3 reactions and be able to send one back, including
	// as a tool call targeting an older message identified by its msgid.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	var calls atomic.Int32
	var toolsSeen atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if _, ok := body["tools"]; ok {
			toolsSeen.Store(true)
		}
		fmt.Fprint(w, sseToolCall("Sure", reactToolName, `{"emoji":"😏","msgid":"m-alice"}`, "call_1"))
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.AllowedAccounts = []string{"alice"}
	c.Bot.Reactions.Enabled = true
	c.Bot.Reactions.Spontaneous = false
	c.Bot.Reactions.MaxPerReply = 2
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	waitFor := func(cond func() bool) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	// Incoming reaction: must become conversation context.
	send("@account=alice;+draft/react=🔥;+draft/reply=m-bob :Alice!u@h TAGMSG #chat")
	if !waitFor(func() bool {
		for _, m := range b.history.snapshot("#chat") {
			if strings.Contains(m.Content, "reacted 🔥 to msgid=m-bob") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("incoming reaction not recorded", b.history.snapshot("#chat"))
	}
	// Removing a reaction must also be recorded, so context does not stay stale.
	send("@account=alice;+draft/unreact=🔥;+draft/reply=m-bob :Alice!u@h TAGMSG #chat")
	if !waitFor(func() bool {
		for _, m := range b.history.snapshot("#chat") {
			if strings.Contains(m.Content, "removed reaction 🔥 to msgid=m-bob") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("incoming unreact not recorded", b.history.snapshot("#chat"))
	}
	// Two messages, then an explicit trigger; the model reacts to the first one.
	send("@account=alice;msgid=m-alice :Alice!u@h PRIVMSG #chat :hello from alice")
	send("@account=alice :Alice!u@h PRIVMSG #chat :@Kat react please")
	// Skip any typing indicator TAGMSG; select the reaction specifically.
	got := until("+draft/react=")
	if !strings.Contains(got, "+draft/react=😏") || !strings.Contains(got, "+reply=m-alice") {
		t.Fatal("wrong reaction line", got)
	}
	if !strings.Contains(got, "@+draft/react=") {
		t.Fatal("reaction must be a client-only tag", got)
	}
	until("PRIVMSG #chat")
	if !toolsSeen.Load() {
		t.Fatal("react tool not advertised to the API")
	}
	if calls.Load() != 1 {
		t.Fatal("wrong number of API calls", calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestIRCTypingAndReply(t *testing.T) {
	// Typing indicators must bracket inference, and the first answer line must
	// carry a "+reply" client-only tag pointing at the triggering msgid.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sse("pong answer"))
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.AllowedAccounts = []string{"alice"}
	c.Bot.Typing = true
	c.Bot.Reactions.Enabled = false
	c.Bot.ReplyThreading = "always" // this test asserts auto-threading
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	send("@account=alice;msgid=m-trigger :Alice!u@h PRIVMSG #chat :@Kat hello")
	active := until("+typing=active")
	if !strings.HasPrefix(active, "@+typing=active TAGMSG #chat") {
		t.Fatal("wrong typing-active line", active)
	}
	doneLine := until("+typing=done")
	if !strings.HasPrefix(doneLine, "@+typing=done TAGMSG #chat") {
		t.Fatal("wrong typing-done line", doneLine)
	}
	answer := until("PRIVMSG #chat :")
	if !strings.Contains(answer, "@+reply=m-trigger ") || !strings.Contains(answer, "pong answer") {
		t.Fatal("answer not tagged as a reply", answer)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestIRCPresence(t *testing.T) {
	// Current channel membership, accounts and away state must reach the model
	// through the instructions, without polluting the rolling history.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	var mu sync.Mutex
	var instructions string
	apiStarted := make(chan struct{}, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		mu.Lock()
		instructions, _ = body["instructions"].(string)
		mu.Unlock()
		apiStarted <- struct{}{}
		fmt.Fprint(w, sse("ok"))
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.Typing = false
	c.Bot.Presence = true
	c.Bot.Reactions.Enabled = false
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h, members: newMemberList()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch away-notify account-notify extended-join")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":Alice!u@h JOIN #chat alice :Alice Example")
	send(":Bob!u@h JOIN #chat * :Bob Example")
	send(":server 353 Kat = #chat :Kat Alice Bob")
	send(":server 366 Kat #chat :End NAMES")
	send(":Bob!u@h AWAY :lunch")
	// Barrier: every presence event above must be processed before the trigger.
	send("PING :barrier")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	send("@account=alice;msgid=m1 :Alice!u@h PRIVMSG #chat :@Kat who is here?")
	select {
	case <-apiStarted:
	case <-time.After(8 * time.Second):
		t.Fatal("no API call")
	}
	mu.Lock()
	got := instructions
	mu.Unlock()
	for _, want := range []string{"Alice (account alice)", "Bob (away: lunch)", "2 other member(s)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("presence missing %q in %q", want, got)
		}
	}
	// Presence is advisory prompt state, never conversation history.
	for _, m := range b.history.snapshot("#chat") {
		if strings.Contains(m.Content, "other member") || strings.Contains(m.Content, "away: lunch") {
			t.Fatal("presence leaked into history", m.Content)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestConfigUserValidation(t *testing.T) {
	raw, e := os.ReadFile("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if e := json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	m["irc"].(map[string]any)["user"] = "kat/ergo"
	out, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if e := os.WriteFile(path, out, 0600); e != nil {
		t.Fatal(e)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("invalid irc.user must be rejected")
	}
}

func TestImageConfig(t *testing.T) {
	load := func(t *testing.T, images any) (Config, error) {
		t.Helper()
		raw, e := os.ReadFile("config.example.json")
		if e != nil {
			t.Fatal(e)
		}
		var m map[string]any
		if e := json.Unmarshal(raw, &m); e != nil {
			t.Fatal(e)
		}
		m["bot"].(map[string]any)["images"] = images
		out, e := json.Marshal(m)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if e := os.WriteFile(path, out, 0600); e != nil {
			t.Fatal(e)
		}
		return loadConfig(path)
	}
	if _, err := load(t, map[string]any{"enabled": true}); err == nil {
		t.Fatal("images.model must be required when enabled")
	}
	if _, err := load(t, map[string]any{"enabled": true, "model": "gpt-image-1", "filehost": "ftp://host/x"}); err == nil {
		t.Fatal("a non-http filehost must be rejected")
	}
	// config.example.json uses chatgpt auth, which cannot generate images.
	if _, err := load(t, map[string]any{"enabled": true, "model": "gpt-image-1"}); err == nil {
		t.Fatal("chatgpt auth must require a separate images.api_key")
	}
	c, err := load(t, map[string]any{"enabled": true, "model": "gpt-image-1", "api_key": "sk-img"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Bot.Images.Size != "1024x1024" || c.Bot.Images.MaxPerReply != 1 || c.Bot.Images.TimeoutSeconds != 120 {
		t.Fatal("unexpected image defaults", c.Bot.Images)
	}
	if c.Bot.Images.APIKey != "sk-img" {
		t.Fatal("images.api_key not preserved", c.Bot.Images.APIKey)
	}
}

func TestIRCImageUpload(t *testing.T) {
	// The model requests an image; Kat generates it via the Images API, uploads
	// the bytes to the server file host, and posts the resolved URL as a reply.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()

	want := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	var uploaded atomic.Value
	filehost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("filehost method %q", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("filehost content-type %q", ct)
		}
		if cd := r.Header.Get("Content-Disposition"); !strings.Contains(cd, "kat.png") {
			t.Errorf("filehost disposition %q", cd)
		}
		body, _ := io.ReadAll(r.Body)
		uploaded.Store(body)
		w.Header().Set("Location", "/upload/abc.png")
		w.WriteHeader(http.StatusCreated)
	}))
	defer filehost.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			fmt.Fprint(w, sseToolCall("", imageToolName, `{"prompt":"a cat astronaut"}`, "call_img"))
		case "/images/generations":
			var body map[string]any
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
			if body["prompt"] != "a cat astronaut" || body["model"] != "gpt-image-1" {
				t.Errorf("unexpected images request: %v", body)
			}
			resp, _ := json.Marshal(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(want)}}})
			w.Write(resp)
		default:
			t.Errorf("unexpected API path %q", r.URL.Path)
		}
	}))
	defer api.Close()

	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.Typing = false
	c.Bot.Presence = false
	c.Bot.Reactions.Enabled = false
	c.Bot.Images.Enabled = true
	c.Bot.Images.Model = "gpt-image-1"
	c.Bot.Images.MaxPerReply = 1
	c.Bot.Images.TimeoutSeconds = 30
	c.Bot.ReplyThreading = "always" // assert the image URL is threaded too
	// A plain client can reach both mock servers over HTTP.
	a := &AI{cfg: c, http: &http.Client{}, endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	// soju ends the ISUPPORT trailing parameter with "are supported", which girc
	// ignores; the filehost must still be discovered.
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii soju.im/FILEHOST=" + filehost.URL + " :are supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	send("@account=alice;msgid=m-img :Alice!u@h PRIVMSG #chat :@Kat draw a cat")
	// girc omits the optional trailing ":" for a single-token message, so match
	// on the command/target rather than "PRIVMSG #chat :".
	got := until("PRIVMSG #chat ")
	if !strings.Contains(got, "@+reply=m-img ") || !strings.Contains(got, "/upload/abc.png") {
		t.Fatal("image URL not posted as a reply", got)
	}
	body, _ := uploaded.Load().([]byte)
	if !bytes.Equal(body, want) {
		t.Fatal("uploaded bytes differ", body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestMemberList(t *testing.T) {
	m := newMemberList()
	m.join("#a", "Alice", "alice")
	m.join("#a", "Bob", "")
	m.setAway("Bob", "lunch")
	m.setAccount("Bob", "bob")
	m.join("#a", "Kat", "")
	got := m.summary("#a", "Kat")
	if len(got) != 2 || got[0].nick != "Alice" || got[0].account != "alice" ||
		got[1].nick != "Bob" || got[1].away != "lunch" || got[1].account != "bob" {
		t.Fatal("summary", got)
	}
	m.rename("Bob", "Robert")
	if got := m.summary("#a", "Kat"); len(got) != 2 || got[1].nick != "Robert" {
		t.Fatal("rename", got)
	}
	m.removeAll("Robert")
	if got := m.summary("#a", "Kat"); len(got) != 1 || got[0].nick != "Alice" {
		t.Fatal("removeAll", got)
	}
	m.clear("#a")
	if got := m.summary("#a", "Kat"); len(got) != 0 {
		t.Fatal("clear", got)
	}
}

func TestIRCSASLNotRepeated(t *testing.T) {
	// girc re-sends AUTHENTICATE on any later CAP ACK, which soju triggers via
	// CAP NEW (cap-notify). The bot must complete SASL once and not re-auth.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.IRC.SASLUser = "kat/ergo"
	c.IRC.SASLPassword = "secret"
	c.Bot.HistoryFile = ""
	a := &AI{cfg: c, http: &http.Client{}}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :sasl message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("AUTHENTICATE PLAIN")
	send("AUTHENTICATE +")
	until("AUTHENTICATE ")
	send(":server 903 Kat :SASL authentication successful")
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	// soju connected upstream and announces new caps via cap-notify.
	send(":server CAP Kat NEW account-notify")
	newReq := until("CAP REQ ")
	newCaps := strings.TrimPrefix(strings.TrimPrefix(newReq, "CAP REQ "), ":")
	if !strings.Contains(newCaps, "account-notify") {
		t.Fatal("expected CAP REQ for account-notify", newReq)
	}
	send(":server CAP Kat ACK :" + newCaps)
	// A second AUTHENTICATE here is the reconnect-loop bug.
	conn.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	for scan.Scan() {
		if strings.Contains(scan.Text(), "AUTHENTICATE") {
			t.Fatalf("client re-authenticated after CAP NEW: %q", scan.Text())
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestUploadCredentials(t *testing.T) {
	b := &Bot{}
	if u, p := b.uploadCredentials(); u != "" || p != "" {
		t.Fatal("empty config must yield no credentials", u, p)
	}
	var c Config
	c.IRC.SASLUser = "kat/ergo"
	c.IRC.SASLPassword = "saslpass"
	b = &Bot{cfg: c}
	if u, p := b.uploadCredentials(); u != "kat/ergo" || p != "saslpass" {
		t.Fatal("SASL must take precedence", u, p)
	}
	c.IRC.SASLUser, c.IRC.SASLPassword = "", ""
	c.IRC.User, c.IRC.Password = "kat/ergo", "passpass"
	b = &Bot{cfg: c}
	if u, p := b.uploadCredentials(); u != "kat/ergo" || p != "passpass" {
		t.Fatal("PASS credentials fallback", u, p)
	}
	c.IRC.Password = ""
	if u, _ := (&Bot{cfg: c}).uploadCredentials(); u != "" {
		t.Fatal("missing password must yield no credentials", u)
	}
}

func TestFilehostFromISupport(t *testing.T) {
	soju := girc.Event{Params: []string{"*", "CHANTYPES=#", "soju.im/FILEHOST=https://soju.example/uploads", "are supported"}}
	if got := filehostFromISupport(soju); got != "https://soju.example/uploads" {
		t.Fatal("soju token not found", got)
	}
	ergo := girc.Event{Params: []string{"*", "draft/FILEHOST=https://ergo.example/files", "are supported by this server"}}
	if got := filehostFromISupport(ergo); got != "https://ergo.example/files" {
		t.Fatal("ergo token not found", got)
	}
	if got := filehostFromISupport(girc.Event{Params: []string{"*", "CHANTYPES=#"}}); got != "" {
		t.Fatal("unexpected token", got)
	}
}

func TestIRCImageNoFilehost(t *testing.T) {
	// When the model asks for an image but no upload host is known, the bot must
	// say so instead of failing silently.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			fmt.Fprint(w, sseToolCall("", imageToolName, `{"prompt":"a cat"}`, "call_img"))
		case "/images/generations":
			resp, _ := json.Marshal(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString([]byte("png"))}}})
			w.Write(resp)
		default:
			t.Errorf("unexpected API path %q", r.URL.Path)
		}
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.Typing = false
	c.Bot.Presence = false
	c.Bot.Reactions.Enabled = false
	c.Bot.ReplyThreading = "never"
	c.Bot.Images.Enabled = true
	c.Bot.Images.Model = "gpt-image-1"
	c.Bot.Images.TimeoutSeconds = 30
	a := &AI{cfg: c, http: &http.Client{}, endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	// No FILEHOST token is advertised.
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :are supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	send("@account=alice;msgid=m1 :Alice!u@h PRIVMSG #chat :@Kat draw a cat")
	got := until("couldn't generate that image")
	if !strings.Contains(got, "PRIVMSG #chat") {
		t.Fatal("fallback not posted to channel", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestIRCReplyTool(t *testing.T) {
	// In "model" mode the answer is threaded only when the model asks for it.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, sseToolCall("hi alice", replyToolName, `{"msgid":"m1"}`, "c1"))
			return
		}
		fmt.Fprint(w, sse("hi everyone"))
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.Typing = false
	c.Bot.Presence = false
	c.Bot.Reactions.Enabled = false
	c.Bot.Images.Enabled = false
	c.Bot.CooldownSeconds = 0
	c.Bot.ReplyThreading = "model"
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch")
	req := until("CAP REQ :")
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	// Model asks to thread: the answer carries +reply.
	send("@account=alice;msgid=m1 :Alice!u@h PRIVMSG #chat :@Kat hi")
	got := until("PRIVMSG #chat")
	if !strings.Contains(got, "@+reply=m1 ") || !strings.Contains(got, "hi alice") {
		t.Fatal("answer should be threaded", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for b.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Model stays silent about threading: the answer addresses the channel.
	send("@account=alice;msgid=m2 :Alice!u@h PRIVMSG #chat :@Kat hi all")
	got2 := until("PRIVMSG #chat")
	if strings.Contains(got2, "+reply") || !strings.Contains(got2, "hi everyone") {
		t.Fatal("answer should not be threaded", got2)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestOwnMessages(t *testing.T) {
	o := newOwnMessages(2)
	o.add("#a", "m1", "one")
	o.add("#a", "m2", "two")
	o.add("#a", "m3", "three")
	if o.has("#a", "m1") || !o.has("#a", "m2") || !o.has("#a", "m3") {
		t.Fatal("oldest message must be evicted at the limit")
	}
	if o.has("#a", "") || o.has("#b", "m3") {
		t.Fatal("must be non-empty and per-channel")
	}
	got := o.recent("#a", 5)
	if len(got) != 2 || got[1].id != "m3" {
		t.Fatal(got)
	}
}

func TestIRCRedaction(t *testing.T) {
	// Kat learns the msgid of its own message from echo-message, then retracts it
	// with REDACT when the model asks. Only its own tracked msgids are accepted.
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, sse("hello from kat"))
			return
		}
		fmt.Fprint(w, sseToolCall("", redactToolName, `{"msgid":"own1","reason":"wrong"}`, "call_r"))
	}))
	defer api.Close()
	t.Setenv("OPENAI_API_KEY", "test")
	c, e := loadConfig("config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	c.OpenAI.Auth = "api_key"
	c.IRC.TLS = false
	c.IRC.Server = "127.0.0.1"
	c.IRC.Port = listener.Addr().(*net.TCPAddr).Port
	c.Bot.HistoryFile = ""
	c.Bot.Typing = false
	c.Bot.Presence = false
	c.Bot.Reactions.Enabled = false
	c.Bot.Images.Enabled = false
	c.Bot.CooldownSeconds = 0
	c.Bot.ReplyThreading = "never"
	c.Bot.Redaction = true
	a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: a, history: h, own: newOwnMessages(50)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	scan := bufio.NewScanner(conn)
	send := func(s string) {
		t.Helper()
		if _, e := fmt.Fprint(conn, s+"\r\n"); e != nil {
			t.Fatal(e)
		}
	}
	until := func(sub string) string {
		t.Helper()
		for scan.Scan() {
			l := scan.Text()
			if strings.Contains(l, sub) {
				return l
			}
		}
		t.Fatalf("missing %q: %v", sub, scan.Err())
		return ""
	}
	until("CAP LS")
	send(":server CAP * LS :message-tags server-time account-tag batch echo-message draft/message-redaction")
	req := until("CAP REQ :")
	if !strings.Contains(req, "echo-message") || !strings.Contains(req, "draft/message-redaction") {
		t.Fatal("redaction caps not requested", req)
	}
	send(":server CAP Kat ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
	until("CAP END")
	send(":server 001 Kat :Welcome")
	send(":server 005 Kat CHANTYPES=# CASEMAPPING=ascii :supported")
	send(":server 376 Kat :End MOTD")
	until("JOIN #chat")
	send(":Kat!kat@localhost JOIN #chat")
	send(":server 353 Kat = #chat :Kat Alice")
	send(":server 366 Kat #chat :End NAMES")
	send("PING :joined")
	until("PONG")
	if !b.ready.Load() {
		t.Fatal("not ready after JOIN")
	}
	// The bot answers; the server echoes it back with a msgid.
	send("@account=alice;msgid=t1 :Alice!u@h PRIVMSG #chat :@Kat say hi")
	until("PRIVMSG #chat")
	send("@msgid=own1 :Kat!kat@localhost PRIVMSG #chat :hello from kat")
	send("PING :echo")
	until("PONG")
	deadline := time.Now().Add(3 * time.Second)
	for b.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The model now retracts its own message.
	send("@account=alice;msgid=t2 :Alice!u@h PRIVMSG #chat :@Kat that was wrong, remove it")
	// girc omits the optional trailing ":" for the single-token reason.
	got := until("REDACT #chat own1")
	if !strings.Contains(got, "wrong") {
		t.Fatal("redaction missing reason", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stuck")
	}
}
