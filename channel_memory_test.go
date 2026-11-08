package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testChannelMemory(t *testing.T, channels ...string) *ChannelMemory {
	t.Helper()
	m, err := newChannelMemory(ChannelMemoryConfig{Enabled: true, File: filepath.Join(t.TempDir(), "channel-memory.json"), MaxBytes: 8192, BatchMessages: 2, IntervalSeconds: 60}, channels)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestChannelMemoryWholeFileBoundAndIsolation(t *testing.T) {
	m := testChannelMemory(t, "#one", "#two")
	now := time.Now().UTC()
	for i := 0; i < 500; i++ {
		for _, ch := range []string{"#one", "#two"} {
			m.observe(ch, Message{Role: "user", Content: strings.Repeat("é<\"😀", 200)}, now)
		}
	}
	m.observe("#other", Message{Role: "user", Content: "must not be stored"}, now)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.cfg.File)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > m.cfg.MaxBytes {
		t.Fatal("file exceeded hard byte limit", len(raw))
	}
	for _, ch := range []string{"#one", "#two"} {
		if m.doc.Channels[ch].Dropped == 0 {
			t.Fatal("backlog overflow not recorded")
		}
		if len(m.doc.Channels[ch].Pending) == 0 {
			t.Fatal("lost all recent observations")
		}
	}
	again, err := newChannelMemory(m.cfg, []string{"#one", "#two"})
	if err != nil {
		t.Fatal(err)
	}
	again.doc.Channels["#one"].Memory = "Only one"
	if again.summary("#two") != "" || again.summary("#other") != "" {
		t.Fatal("cross-channel memory leak")
	}
	if len(again.doc.Channels) != 2 {
		t.Fatal("unconfigured channel was stored")
	}
}
func TestChannelMemoryRewriteAndArrivalsDuringCuration(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	now := time.Now().UTC()
	m.doc.Channels["#chat"].Memory = "Old important event; outdated detail."
	m.observe("#chat", Message{Role: "user", Content: "a correction"}, now)
	m.observe("#chat", Message{Role: "user", Content: "a memorable new event"}, now)
	job, err := m.nextJob(now)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	if job.previous != "Old important event; outdated detail." {
		t.Fatal("previous memory missing from rewrite")
	}
	m.observe("#chat", Message{Role: "user", Content: "arrived during model request"}, now.Add(time.Second))
	if err = m.commit(job, "Old important event; corrected detail; new event.", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	row := m.doc.Channels["#chat"]
	if strings.Contains(row.Memory, "outdated") || len(row.Pending) != 1 || row.Pending[0].Message.Content != "arrived during model request" {
		t.Fatal(row)
	}
	restored, err := newChannelMemory(m.cfg, []string{"#chat"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.summary("#chat") != row.Memory || len(restored.doc.Channels["#chat"].Pending) != 1 {
		t.Fatal("state not persisted together")
	}
	if job, err := restored.nextJob(now.Add(3 * time.Second)); err != nil || job != nil {
		t.Fatal("retried too soon", job, err)
	}
	if job, err := restored.nextJob(now.Add(62 * time.Second)); err != nil || job != nil {
		t.Fatal("idle tail triggered below batch threshold", job, err)
	}
	restored.observe("#chat", Message{Role: "user", Content: "second new observation"}, now.Add(62*time.Second))
	if job, err := restored.nextJob(now.Add(62 * time.Second)); err != nil || job == nil {
		t.Fatal("full new batch was not processed", job, err)
	}
}
func TestChannelMemoryFailedCommitPreservesState(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	now := time.Now().UTC()
	m.doc.Channels["#chat"].Memory = "Important old memory"
	for i := 0; i < 2; i++ {
		m.observe("#chat", Message{Role: "user", Content: "new"}, now)
	}
	job, err := m.nextJob(now)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(m.cfg.File)
	if err = m.commit(job, strings.Repeat("x", job.budget+1), now); err == nil {
		t.Fatal("oversized memory accepted")
	}
	after, _ := os.ReadFile(m.cfg.File)
	if string(after) != string(before) {
		t.Fatal("oversized answer changed disk")
	}
	savedPath := m.cfg.File
	m.cfg.File = t.TempDir() // Atomic rename onto a directory fails even as root.
	if err = m.commit(job, "replacement", now); err == nil {
		t.Fatal("simulated storage failure did not fail")
	}
	m.cfg.File = savedPath
	if m.summary("#chat") != "Important old memory" || len(m.doc.Channels["#chat"].Pending) != 2 {
		t.Fatal("failed write discarded old memory or pending batch")
	}
}
func TestChannelMemorySeedOnlyOnceAndHistoryIndependent(t *testing.T) {
	h, _ := newHistory("", 2)
	h.add("#chat", Message{Role: "user", Content: "first"})
	h.add("#chat", Message{Role: "user", Content: "second"})
	m := testChannelMemory(t, "#chat")
	now := time.Now().UTC()
	if err := m.seed(h, now); err != nil {
		t.Fatal(err)
	}
	if err := m.seed(h, now); err != nil {
		t.Fatal(err)
	}
	if len(m.doc.Channels["#chat"].Pending) != 2 {
		t.Fatal("seed repeated on same object")
	}
	h.add("#chat", Message{Role: "user", Content: "third"})
	m.observe("#chat", Message{Role: "user", Content: "third"}, now)
	if len(h.snapshot("#chat")) != 2 || len(m.doc.Channels["#chat"].Pending) != 3 {
		t.Fatal("short history eviction lost long-term staging")
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	restored, err := newChannelMemory(m.cfg, []string{"#chat"})
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.seed(h, now); err != nil {
		t.Fatal(err)
	}
	if len(restored.doc.Channels["#chat"].Pending) != 3 {
		t.Fatal("restart duplicated seeded observations")
	}
}
func TestChannelMemoryConcurrentObserveCommitSave(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		m.observe("#chat", Message{Role: "user", Content: "before"}, now)
	}
	job, err := m.nextJob(now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.observe("#chat", Message{Role: "user", Content: "during"}, now)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = m.summary("#chat")
			if err := m.save(); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		if err := m.commit(job, "selected memory", now); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(m.cfg.File)
	if len(raw) > m.cfg.MaxBytes {
		t.Fatal("concurrent writes exceeded bound")
	}
}
func TestChannelMemoryInvalidFileIsNotOverwritten(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	for _, raw := range []string{"bad json", `{"version":99,"channels":{}}`, strings.Repeat("x", m.cfg.MaxBytes+1)} {
		if err := os.WriteFile(m.cfg.File, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newChannelMemory(m.cfg, []string{"#chat"}); err == nil {
			t.Fatal("invalid file accepted")
		}
		after, _ := os.ReadFile(m.cfg.File)
		if string(after) != raw {
			t.Fatal("invalid file overwritten")
		}
	}
}
func TestChannelMemoryConfigDefaultsAndPathProtection(t *testing.T) {
	c := exampleConfig(t)
	c.Bot.ChannelMemory = ChannelMemoryConfig{Enabled: true}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := atomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bot.ChannelMemory.File != filepath.Join(filepath.Dir(path), "channel-memory.json") || got.Bot.ChannelMemory.MaxBytes != 65536 {
		t.Fatal("wrong memory defaults")
	}
	for _, dest := range []string{"config.json", "oauth.json", "history.json", "memory.md"} {
		c.Bot.MemoryFile = ""
		if dest == "memory.md" {
			c.Bot.MemoryFile = "memory.md"
		}
		c.Bot.ChannelMemory.File = dest
		if err = atomicJSON(path, c); err != nil {
			t.Fatal(err)
		}
		if _, err = loadConfig(path); err == nil {
			t.Fatalf("accepted protected path %s", dest)
		}
	}
	c.Bot.MemoryFile = ""
	c.Bot.ChannelMemory.File = "other.json"
	c.Bot.ChannelMemory.MaxBytes = 1024
	if err = atomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	if _, err = loadConfig(path); err == nil {
		t.Fatal("unusable byte budget accepted")
	}
}
func TestChannelMemoryCurationRequestAndIsolation(t *testing.T) {
	for _, auth := range []string{"api_key", "chatgpt"} {
		t.Run(auth, func(t *testing.T) {
			var c Config
			c.OpenAI.Auth = auth
			c.OpenAI.APIKey = "test"
			c.OpenAI.WebSearch = true
			c.Bot.Reactions.Enabled = true
			c.Bot.Images.Enabled = true
			c.Bot.Redaction = true
			c.Bot.Persona = "PRIVATE PERSONA MUST NOT ENTER CURATION"
			c.OpenAI.CredentialsFile = filepath.Join(t.TempDir(), "oauth.json")
			if err := atomicJSON(c.OpenAI.CredentialsFile, &Credentials{ClientID: "issued", Subject: "test", Scope: planScope, AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if _, ok := body["tools"]; ok {
					t.Error("tools offered to memory curator")
				}
				if body["instructions"] != channelMemoryInstructions {
					t.Error("persona instructions leaked into curation")
				}
				text := body["text"].(map[string]any)
				format := text["format"].(map[string]any)
				if format["type"] != "json_schema" || format["strict"] != true {
					t.Error("missing strict memory schema")
				}
				_, max := body["max_output_tokens"]
				if max != (auth == "api_key") {
					t.Error("wrong OAuth/API budget")
				}
				fmt.Fprint(w, sse(`{"memory":"Retained event and corrected fact."}`))
			}))
			defer server.Close()
			a := &AI{cfg: c, http: server.Client(), endpoint: server.URL}
			job := &memoryJob{previous: "old fact", observations: []memoryObservation{{Sequence: 1, Message: Message{Role: "user", Content: "correction"}}}, budget: 2000}
			got, err := a.curateChannelMemory(context.Background(), job)
			if err != nil || got != "Retained event and corrected fact." {
				t.Fatal(got, err)
			}
		})
	}
}
func TestChannelMemoryIncludedAsDataNotInstructions(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, sse("answer"))
	}))
	defer server.Close()
	var c Config
	c.OpenAI.Auth = "api_key"
	c.OpenAI.APIKey = "test"
	c.Bot.Persona = "persona"
	a := &AI{cfg: c, http: server.Client(), endpoint: server.URL}
	if _, err := a.reply(context.Background(), []Message{{Role: "user", Content: "question"}}, replyOptions{channelMemory: "untrusted remembered fact"}); err != nil {
		t.Fatal(err)
	}
	input := body["input"].([]any)
	if len(input) != 2 || input[0].(map[string]any)["role"] != "user" || !strings.Contains(input[0].(map[string]any)["content"].(string), "untrusted remembered fact") {
		t.Fatal("missing untrusted data context", body)
	}
	if strings.Contains(body["instructions"].(string), "untrusted remembered fact") {
		t.Fatal("memory promoted to developer instructions")
	}
}

func TestChannelMemoryStrictThresholdAndCooldown(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	m.cfg.MaxBytes = 65536
	m.cfg.BatchMessages = 40
	now := time.Now().UTC()
	for i := 0; i < 39; i++ {
		m.observe("#chat", Message{Role: "user", Content: "new observation"}, now)
	}
	if job, err := m.nextJob(now.Add(7 * 24 * time.Hour)); err != nil || job != nil {
		t.Fatal("silence bypassed 40-observation threshold", job, err)
	}
	m.observe("#chat", Message{Role: "user", Content: "number 40"}, now.Add(7*24*time.Hour))
	job, err := m.nextJob(now.Add(7 * 24 * time.Hour))
	if err != nil || job == nil {
		t.Fatal("full batch did not trigger", job, err)
	}
	// A failed attempt leaves all observations new, but cooldown still applies.
	if retry, err := m.nextJob(now.Add(7*24*time.Hour + 30*time.Second)); err != nil || retry != nil {
		t.Fatal("failed attempt bypassed cooldown", retry, err)
	}
	if err = m.commit(job, "selected long-term facts", now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		m.observe("#chat", Message{Role: "user", Content: "next batch"}, now.Add(7*24*time.Hour+30*time.Second))
	}
	if job, err := m.nextJob(now.Add(7*24*time.Hour + 30*time.Second)); err != nil || job != nil {
		t.Fatal("full batch bypassed cooldown", job, err)
	}
	if job, err := m.nextJob(now.Add(7*24*time.Hour + 61*time.Second)); err != nil || job == nil {
		t.Fatal("eligible batch did not trigger after cooldown", job, err)
	}
}

func TestChannelMemoryPressureCannotBypassThreshold(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	m.cfg.BatchMessages = 40
	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		m.observe("#chat", Message{Role: "user", Content: strings.Repeat("<", 2000)}, now)
	}
	if m.doc.Channels["#chat"].Dropped == 0 {
		t.Fatal("test did not fill bounded queue")
	}
	if job, err := m.nextJob(now.Add(24 * time.Hour)); err != nil || job != nil {
		t.Fatal("byte pressure bypassed count threshold", job, err)
	}
	for i := 20; i < 40; i++ {
		m.observe("#chat", Message{Role: "user", Content: strings.Repeat("<", 2000)}, now)
	}
	job, err := m.nextJob(now.Add(24 * time.Hour))
	if err != nil || job == nil {
		t.Fatal("bounded queue prevented a busy channel from reaching threshold", job, err)
	}
	if len(job.observations) >= 40 {
		t.Fatal("test should exercise a retained subset after overflow")
	}
}

func TestChannelMemoryLegacyCheckpointMigration(t *testing.T) {
	m := testChannelMemory(t, "#chat")
	m.cfg.BatchMessages = 40
	now := time.Now().UTC()
	old := map[string]any{"version": 1, "channels": map[string]any{"#chat": map[string]any{"memory": "old selected memory", "next_sequence": 500, "pending": []memoryObservation{{Sequence: 500, ObservedAt: now, Message: Message{Role: "user", Content: "only one new observation"}}}}}}
	if err := atomicJSON(m.cfg.File, old); err != nil {
		t.Fatal(err)
	}
	restored, err := newChannelMemory(m.cfg, []string{"#chat"})
	if err != nil {
		t.Fatal(err)
	}
	if *restored.doc.Channels["#chat"].ProcessedThrough != 499 {
		t.Fatal("lifetime sequence was mistaken for new count")
	}
	if job, err := restored.nextJob(now.Add(7 * 24 * time.Hour)); err != nil || job != nil {
		t.Fatal("legacy migration triggered premature curation", job, err)
	}
	for i := 0; i < 39; i++ {
		restored.observe("#chat", Message{Role: "user", Content: "new"}, now)
	}
	if job, err := restored.nextJob(now); err != nil || job == nil {
		t.Fatal("migrated new count did not reach threshold", job, err)
	}
}
