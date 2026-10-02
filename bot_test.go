package main

import (
	"bufio"
	"context"
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
	h.add("#a", Message{"user", "one"})
	h.add("#a", Message{"assistant", "two"})
	h.add("#a", Message{"user", "three"})
	h.add("#b", Message{"user", "private"})
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
	if stat, e := os.Stat(path); e != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("history permissions")
	}
}
func sse(text string) string {
	b, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
	return "data: " + string(b) + "\n\ndata: {\"type\":\"response.completed\"}\n\n"
}
func TestSSE(t *testing.T) {
	out, e := consumeSSE(strings.NewReader(sse("Hello 😏")))
	if e != nil || out != "Hello 😏" {
		t.Fatal(out, e)
	}
	for _, in := range []string{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "data: {\"type\":\"response.failed\"}\n\n", "data: {\"type\":\"response.incomplete\"}\n\n", "data: bad\n\n", "data: {\"type\":\"response.completed\"}\n\n"} {
		if _, e := consumeSSE(strings.NewReader(in)); e == nil {
			t.Fatal("accepted incomplete stream", in)
		}
	}
}
func TestOpenAIRequest(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	for _, mode := range []string{"api_key", "chatgpt"} {
		t.Run(mode, func(t *testing.T) {
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
				_, hasMax := body["max_output_tokens"]
				if hasMax != (mode == "api_key") {
					t.Error("wrong max_output_tokens for auth mode")
				}
				fmt.Fprint(w, sse("Test"))
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
			a := &AI{cfg: c, http: server.Client(), endpoint: server.URL}
			if out, e := a.reply(context.Background(), []Message{{"user", "hello"}}); e != nil || out != "Test" {
				t.Fatal(out, e)
			}
		})
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
