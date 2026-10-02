package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func exampleConfig(t *testing.T) Config {
	t.Helper()
	data, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err = json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestWaitForSetupAndHealth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan *Bot, 1)
	go func() {
		b, err := waitForSetup(ctx, path, 5*time.Millisecond)
		if err == nil {
			ready <- b
		}
	}()
	var current atomic.Pointer[Bot]
	handler := healthHandler(&current)
	check := func(path string, status int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != status {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	check("/healthz", 200)
	check("/readyz", 503)
	c := exampleConfig(t)
	c.OpenAI.Auth = "api_key"
	c.OpenAI.APIKey = ""
	c.Bot.PersonaFile = "persona.md"
	if err := atomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte("A generic persona."), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
		t.Fatal("started without credentials")
	case <-time.After(25 * time.Millisecond):
	}
	c.OpenAI.APIKey = "test-key"
	if err := atomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-ready:
		current.Store(b)
		check("/readyz", 503)
		b.ready.Store(true)
		check("/readyz", 200)
		if b.cfg.Bot.HistoryFile != filepath.Join(dir, "history.json") {
			t.Fatal("wrong relative path")
		}
	case <-time.After(time.Second):
		t.Fatal("setup did not resume automatically")
	}
}
func TestWaitForSetupCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitForSetup(ctx, filepath.Join(t.TempDir(), "missing.json"), time.Hour); err != context.Canceled {
		t.Fatal(err)
	}
}
func TestConfigCredentialsAndValidation(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "environment-key")
	c := exampleConfig(t)
	c.OpenAI.Auth = "api_key"
	c.OpenAI.APIKey = "file-key"
	c.IRC.TLS = false
	c.IRC.SASLUser = "bot"
	c.IRC.SASLPassword = "password"
	path := filepath.Join(t.TempDir(), "config.json")
	write := func() {
		t.Helper()
		if err := atomicJSON(path, c); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := loadConfig(path); err == nil {
		t.Fatal("plaintext password accepted without opt-in")
	}
	c.IRC.AllowPlaintextAuth = true
	write()
	got, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenAI.APIKey != "file-key" || got.IRC.SASLPassword != "password" {
		t.Fatal("file credentials lost")
	}
	c.IRC.Channels = []string{"#chat", "#CHAT"}
	write()
	if _, err := loadConfig(path); err == nil {
		t.Fatal("duplicate channels accepted")
	}
}
func TestOAuthReloadsAfterLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.json")
	c := &Credentials{ClientID: "issued", Subject: "test", Scope: planScope, AccessToken: "first", ExpiresAt: time.Now().Add(time.Hour)}
	for _, token := range []string{"first", "after-login"} {
		c.AccessToken = token
		if err := atomicJSON(path, c); err != nil {
			t.Fatal(err)
		}
		got, err := oauthToken(context.Background(), path)
		if err != nil || got != token {
			t.Fatal(got, err)
		}
	}
}
