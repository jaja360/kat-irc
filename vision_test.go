package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func testVision() VisionConfig { v := VisionConfig{Enabled: true}; _ = v.defaults(); return v }
func TestVisionDownloadPolicy(t *testing.T) {
	data := testPNG(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials leaked to image host")
		}
		switch r.URL.Path {
		case "/bad.png":
			fmt.Fprint(w, "<html>not an image</html>")
		case "/big.png":
			w.Header().Set("Content-Length", "99999999")
		case "/chunked.png":
			w.(http.Flusher).Flush()
			_, _ = w.Write(bytes.Repeat([]byte{'a'}, 2048))
		case "/redirect.png":
			http.Redirect(w, r, "http://localhost:1/no.png", 302)
		case "/same.png":
			http.Redirect(w, r, "/ok.png", 302)
		case "/loop.png":
			http.Redirect(w, r, "/loop.png", 302)
		case "/missing.png":
			w.WriteHeader(404)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
		}
	}))
	defer srv.Close()
	v := testVision()
	if _, err := v.fetch(context.Background(), srv.URL+"/ok.png", ""); err == nil || hits.Load() != 0 {
		t.Fatal("private address fetched without trust")
	}
	v.AllowPrivateOrigins = []string{srv.URL}
	got, err := v.fetch(context.Background(), srv.URL+"/same.png", "")
	if err != nil || got != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data) {
		t.Fatal(got, err)
	}
	for _, p := range []string{"bad.png", "big.png", "redirect.png", "loop.png", "missing.png"} {
		if _, err := v.fetch(context.Background(), srv.URL+"/"+p, ""); err == nil {
			t.Error("accepted", p)
		}
	}
	v.MaxImageBytes = 1024
	if _, err := v.fetch(context.Background(), srv.URL+"/chunked.png", ""); err == nil {
		t.Fatal("accepted excessive chunked response")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.fetch(ctx, srv.URL+"/ok.png", ""); err == nil {
		t.Fatal("ignored cancellation")
	}
	// Advertised filehost may be internal; trust is exact origin, not suffix.
	v.AllowPrivateOrigins = nil
	u, _ := url.Parse(srv.URL + "/uploads/no-extension")
	if !v.trusted(u, srv.URL+"/uploads") || v.trusted(u, "http://example.org/uploads") {
		t.Fatal("incorrect filehost trust")
	}
}
func TestVisionAddressAndCandidateValidation(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "fc00::1", "0.0.0.0", "192.0.2.1", "64:ff9b::a00:1"} {
		if publicImageIP(netip.MustParseAddr(raw)) {
			t.Error("allowed nonpublic IP", raw)
		}
	}
	if !publicImageIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
	v := testVision()
	got := v.candidates("look <https://example.org/photo.PNG?x=1> and https://example.org/page and http://a:b@example.org/x.png", "")
	if len(got) != 1 || got[0] != "https://example.org/photo.PNG?x=1" {
		t.Fatal(got)
	}
	if len(v.candidates("http://files.internal/uploads/abc", "http://files.internal/uploads")) != 1 {
		t.Fatal("extensionless filehost not recognized")
	}
	for _, origin := range []string{"http://user:pass@host", "http://host/path", "http://host?x=1", "file:///tmp", "http://host/#x"} {
		v := testVision()
		v.AllowPrivateOrigins = []string{origin}
		if v.defaults() == nil {
			t.Error("accepted origin", origin)
		}
	}
}
func TestVisionGIFAndPixelLimits(t *testing.T) {
	var buf bytes.Buffer
	if err := gif.Encode(&buf, image.NewPaletted(image.Rect(0, 0, 2, 2), []color.Color{color.Black}), nil); err != nil {
		t.Fatal(err)
	}
	pngData := testPNG(t)
	// PNG header dimensions are checked before any full image decode/allocation.
	bomb := append([]byte(nil), pngData...)
	binary.BigEndian.PutUint32(bomb[16:20], 100000)
	binary.BigEndian.PutUint32(bomb[20:24], 100000)
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bomb.png" {
			_, _ = w.Write(bomb)
		} else {
			_, _ = w.Write(buf.Bytes())
		}
	}))
	defer srv.Close()
	v := testVision()
	v.AllowPrivateOrigins = []string{srv.URL}
	out, err := v.fetch(context.Background(), srv.URL+"/first.gif", "")
	if err != nil || !strings.HasPrefix(out, "data:image/png;base64,") {
		t.Fatal(out, err)
	}
	if _, err := v.fetch(context.Background(), srv.URL+"/bomb.png", ""); err == nil {
		t.Fatal("accepted excessive dimensions")
	}
}
func TestVisionFirstAnalysisThenPersistentDescription(t *testing.T) {
	for _, auth := range []string{"api_key", "chatgpt"} {
		t.Run(auth, func(t *testing.T) {
			var downloads, analyses, answers atomic.Int32
			data := testPNG(t)
			images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				if r.Header.Get("Authorization") != "" {
					t.Error("API credential sent to image host")
				}
				_, _ = w.Write(data)
			}))
			defer images.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("missing API auth")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded, _ := json.Marshal(body["input"])
				if strings.Contains(string(encoded), "input_image") {
					analyses.Add(1)
					if _, ok := body["tools"]; ok {
						t.Error("tools enabled during description")
					}
					if !strings.Contains(string(encoded), "data:image/png;base64,") {
						t.Error("missing image bytes")
					}
					fmt.Fprint(w, sse("A blue test chart with three labels."))
				} else {
					answers.Add(1)
					if !strings.Contains(string(encoded), "A blue test chart") {
						t.Error("description absent from main answer", string(encoded))
					}
					fmt.Fprint(w, sse("Here is what it shows."))
				}
			}))
			defer api.Close()
			var c Config
			c.OpenAI.Auth = auth
			c.OpenAI.APIKey = "test-key"
			c.OpenAI.Model = "test"
			c.Bot.Persona = "test"
			c.Bot.Vision = testVision()
			c.Bot.Vision.AllowPrivateOrigins = []string{images.URL}
			c.OpenAI.CredentialsFile = filepath.Join(t.TempDir(), "oauth.json")
			if err := atomicJSON(c.OpenAI.CredentialsFile, &Credentials{ClientID: "issued", Subject: "test", Scope: planScope, AccessToken: "test-key", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			a := &AI{cfg: c, http: api.Client(), endpoint: api.URL}
			historyPath := filepath.Join(t.TempDir(), "history.json")
			h, _ := newHistory(historyPath, 40)
			raw := images.URL + "/chart.png"
			h.add("#chat", Message{Role: "user", Content: "look " + raw, MsgID: "image1"})
			h.add("#chat", Message{Role: "user", Content: "Kat: what is this?", MsgID: "question"})
			original := h.snapshot("#chat")
			opts := replyOptions{visionReplyTo: "image1", visionRemember: func(u, d string) { h.describeImage("#chat", u, d) }}
			if _, err := a.reply(context.Background(), original, opts); err != nil {
				t.Fatal(err)
			}
			if len(original[0].Images) != 0 {
				t.Fatal("snapshot was mutated")
			}
			if err := h.save(); err != nil {
				t.Fatal(err)
			}
			persisted, _ := os.ReadFile(historyPath)
			if bytes.Contains(persisted, []byte("base64")) || !bytes.Contains(persisted, []byte("blue test chart")) {
				t.Fatal("wrong persisted image data")
			}
			restored, err := newHistory(historyPath, 40)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.reply(context.Background(), restored.snapshot("#chat"), opts); err != nil {
				t.Fatal(err)
			}
			if downloads.Load() != 1 || analyses.Load() != 1 || answers.Load() != 2 {
				t.Fatal(downloads.Load(), analyses.Load(), answers.Load())
			}
			if len(restored.snapshot("#other")) != 0 {
				t.Fatal("cross-channel history")
			}
		})
	}
}
func TestVisionSelectionAndNoBackgroundFetch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(404) }))
	defer srv.Close()
	var c Config
	c.Bot.Vision = testVision()
	c.Bot.Vision.AllowPrivateOrigins = []string{srv.URL}
	c.Bot.Vision.MaxImages = 1
	a := &AI{cfg: c}
	old := srv.URL + "/old.png"
	recent := srv.URL + "/recent.png"
	direct := srv.URL + "/direct.png"
	messages := []Message{{Role: "user", Content: old, MsgID: "old"}, {Role: "user", Content: recent}, {Role: "user", Content: "Kat: explain"}}
	for _, opts := range []replyOptions{{spontaneous: true}, {memoryUpdate: true}} {
		a.visionInput(context.Background(), messages, opts)
	}
	if hits.Load() != 0 {
		t.Fatal("background request fetched images")
	}
	out := a.visionInput(context.Background(), messages, replyOptions{visionReplyTo: "old"})
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "unavailable") || hits.Load() != 1 {
		t.Fatal(string(b), hits.Load())
	}
	// Direct question attachments take precedence over the replied-to image.
	messages[2].Content += " " + direct
	out = a.visionInput(context.Background(), messages, replyOptions{visionReplyTo: "old"})
	if !strings.Contains(out[2].(map[string]any)["content"].(string), "unavailable") {
		t.Fatal("direct image not selected")
	}
	// Saved descriptions outside the discovery window remain available too.
	messages[0].Images = []ImageDescription{{URL: old, Description: "older saved description"}}
	// Recent descriptions are reused without downloading anything.
	messages[2].Content = "Kat: explain"
	messages[1].Images = []ImageDescription{{URL: recent, Description: "recent cached description"}}
	before := hits.Load()
	out = a.visionInput(context.Background(), messages, replyOptions{})
	if hits.Load() != before || !strings.Contains(out[1].(map[string]any)["content"].(string), "recent cached description") || !strings.Contains(out[0].(map[string]any)["content"].(string), "older saved description") {
		t.Fatal("recent description not reused")
	}
}

func TestVisionTCPReplyTargetAndFilehost(t *testing.T) {
	var downloads, analyses atomic.Int32
	data := testPNG(t)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads.Add(1); _, _ = w.Write(data) }))
	defer files.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		wire, _ := json.Marshal(body["input"])
		if strings.Contains(string(wire), "input_image") {
			analyses.Add(1)
			fmt.Fprint(w, sse("Screenshot of a compiler error."))
		} else {
			if !strings.Contains(string(wire), "Screenshot of a compiler error.") {
				t.Error("image description not available to conversation")
			}
			fmt.Fprint(w, sse("Check the missing semicolon."))
		}
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
	c.Bot.CooldownSeconds = 0
	c.Bot.Vision = testVision()
	c.Bot.Vision.RecentMessages = 1
	h, _ := newHistory("", 40)
	b := &Bot{cfg: c, ai: &AI{cfg: c, http: api.Client(), endpoint: api.URL}, history: h}
	f := startMultilineIRC(t, b, "message-tags batch account-tag server-time")
	f.send(t, ":server 005 Kat soju.im/FILEHOST="+files.URL+"/uploads :are supported by this server")
	// The extensionless URL is outside the recent-message window, but is an explicit reply target.
	f.send(t, "@msgid=picture :Alice!u@h PRIVMSG #chat :"+files.URL+"/uploads/object")
	f.send(t, ":Alice!u@h PRIVMSG #chat :another message")
	f.send(t, "@msgid=question;+reply=picture :Alice!u@h PRIVMSG #chat :Kat: explain this")
	f.until(t, "PRIVMSG #chat ")
	f.send(t, "PING :barrier")
	f.until(t, "PONG")
	if downloads.Load() != 1 || analyses.Load() != 1 {
		t.Fatal(downloads.Load(), analyses.Load())
	}
	found := false
	for _, m := range h.snapshot("#chat") {
		if m.MsgID == "picture" && len(m.Images) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("description not attached to original IRC message")
	}
}
