package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTypingSessionRenewalAndStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var states []string
	ready := make(chan struct{}, 8)
	stop := typingSession(ctx, 20*time.Millisecond, func(s string) { mu.Lock(); states = append(states, s); mu.Unlock(); ready <- struct{}{} })
	for i := 0; i < 3; i++ {
		select {
		case <-ready:
		case <-time.After(time.Second):
			t.Fatal("missing renewal")
		}
	}
	stop()
	stop()
	mu.Lock()
	before := len(states)
	if states[0] != "active" || states[before-1] != "done" {
		t.Error(states)
	}
	mu.Unlock()
	cancel()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(states) != before {
		t.Fatal("notification after done", states)
	}
}
func TestOwnEchoHistoryBeforeAndAfterSend(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "history.json")
			h, _ := newHistory(p, 2)
			original := Message{Role: "assistant", Content: "Here is the image:\nhttps://example.org/p.png"}
			if before {
				h.ownEcho("#chat", "fragment-image", "https://example.org/p.png")
			}
			h.add("#chat", original)
			snapshot := h.snapshot("#chat")
			if !before {
				h.ownEcho("#chat", "fragment-image", "https://example.org/p.png")
			}
			h.ownEcho("#chat", "fragment-caption", "Here is the image:")
			if !h.hasMsgID("#chat", "fragment-image") || !h.hasMsgID("#chat", "fragment-caption") || h.hasMsgID("#other", "fragment-image") {
				t.Fatal("wrong echo association")
			}
			if !before && snapshot[0].hasMsgID("fragment-image") {
				t.Fatal("mutated request snapshot")
			}
			if h.hasUserMsgID("#chat", "fragment-image") {
				t.Fatal("own echo allowed as reaction target")
			}
			if !h.describeImage("#chat", "https://example.org/p.png", "A cat.") {
				t.Fatal("own description rejected")
			}
			if err := h.save(); err != nil {
				t.Fatal(err)
			}
			restored, err := newHistory(p, 2)
			if err != nil {
				t.Fatal(err)
			}
			m := restored.snapshot("#chat")[0]
			if !m.hasMsgID("fragment-image") || len(m.Images) != 1 {
				t.Fatal("lost persisted own image context", m)
			}
			restored.add("#chat", Message{Role: "user", Content: "one"})
			restored.add("#chat", Message{Role: "user", Content: "two"})
			if restored.hasMsgID("#chat", "fragment-image") {
				t.Fatal("echo outlived bounded history")
			}
		})
	}
}

func TestGeneratedImageReplyVisionAndTypingTCP(t *testing.T) {
	data := testPNG(t)
	var downloads, descriptions, responses atomic.Int32
	var files *httptest.Server
	files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.Header().Set("Location", files.URL+"/uploads/p.png")
			w.WriteHeader(201)
			return
		}
		downloads.Add(1)
		_, _ = w.Write(data)
	}))
	defer files.Close()
	analyzing := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/images/generations" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(data)}}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		wire, _ := json.Marshal(body["input"])
		if strings.Contains(string(wire), "input_image") {
			descriptions.Add(1)
			analyzing <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, sse("An actual blue square, not a photograph."))
			return
		}
		n := responses.Add(1)
		if n == 1 {
			fmt.Fprint(w, sseToolCall("", imageToolName, `{"prompt":"a blue square"}`, "img"))
			return
		}
		if !strings.Contains(string(wire), "An actual blue square") {
			t.Error("own image description absent", string(wire))
		}
		fmt.Fprint(w, sse("Yes, the square is blue."))
	}))
	defer func() { unblock(); api.Close() }()
	c, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c.OpenAI.Auth = "api_key"
	c.OpenAI.APIKey = "test"
	c.Bot.Reactions.Enabled = false
	c.Bot.Presence = false
	c.Bot.Typing = true
	c.Bot.CooldownSeconds = 0
	c.Bot.Images.Enabled = true
	c.Bot.Images.Filehost = files.URL
	c.Bot.Vision = testVision()
	c.Bot.Vision.RecentMessages = 1
	c.Bot.Redaction = false
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: &AI{cfg: c, http: api.Client(), endpoint: api.URL}, history: h}
	f := startMultilineIRC(t, b, "message-tags account-tag batch server-time echo-message")
	// Mid-message mention starts generation, without changing the prefix syntax.
	f.send(t, "@msgid=generate :Alice!u@h PRIVMSG #chat :please draw a square @kat")
	line := f.until(t, "PRIVMSG #chat ")
	if !strings.Contains(line, "/uploads/p.png") {
		t.Fatal("no generated image", line)
	}
	f.send(t, "@msgid=generated-image :Kat!kat@host.example PRIVMSG #chat :"+files.URL+"/uploads/p.png")
	f.send(t, "PING :echoed")
	f.until(t, "PONG")
	// The generated assistant image is outside the recent-image window. The echo
	// msgid must resolve it, even with bot.redaction=false.
	f.send(t, "@msgid=comment;+reply=generated-image :Alice!u@h PRIVMSG #chat :what do you think @Kat?")
	select {
	case <-analyzing:
	case <-time.After(5 * time.Second):
		t.Fatal("own image was not analyzed")
	}
	f.until(t, "@+typing=active TAGMSG #chat")
	f.until(t, "@+typing=active TAGMSG #chat") // renewal while analysis is blocked
	unblock()
	f.until(t, "@+typing=done TAGMSG #chat")
	f.until(t, "PRIVMSG #chat ")
	// Wait until the inference's deferred busy reset has completed.
	deadline := time.Now().Add(time.Second)
	for b.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.send(t, "@msgid=again;+reply=generated-image :Alice!u@h PRIVMSG #chat :and now @kat?")
	f.until(t, "PRIVMSG #chat ")
	if downloads.Load() != 1 || descriptions.Load() != 1 || responses.Load() != 3 {
		t.Fatal("own image was rescanned or reply missed", downloads.Load(), descriptions.Load(), responses.Load())
	}
}
