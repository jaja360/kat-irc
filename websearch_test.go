package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebSearchRequestModes(t *testing.T) {
	for _, auth := range []string{"api_key", "chatgpt"} {
		for _, enabled := range []bool{false, true} {
			for _, silent := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/enabled=%v/silent=%v", auth, enabled, silent), func(t *testing.T) {
					var c Config
					c.OpenAI.Auth = auth
					c.OpenAI.APIKey = "test"
					c.OpenAI.WebSearch = enabled
					c.Bot.Reactions.Enabled = true
					c.OpenAI.CredentialsFile = filepath.Join(t.TempDir(), "oauth.json")
					if err := atomicJSON(c.OpenAI.CredentialsFile, &Credentials{ClientID: "issued", Subject: "test", Scope: planScope, AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
						t.Fatal(err)
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var b map[string]any
						if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
							t.Error(err)
						}
						foundWeb, foundReact := false, false
						if ts, ok := b["tools"].([]any); ok {
							for _, v := range ts {
								tool := v.(map[string]any)
								foundWeb = foundWeb || tool["type"] == "web_search"
								foundReact = foundReact || tool["name"] == reactToolName
							}
						}
						if foundWeb != (enabled && !silent) || foundReact == silent {
							t.Error("wrong tool selection", b["tools"])
						}
						_, hasMax := b["max_output_tokens"]
						if hasMax != (auth == "api_key") {
							t.Error("OAuth must omit max_output_tokens")
						}
						if _, ok := b["max_tool_calls"]; ok {
							t.Error("OAuth-unsupported limit sent")
						}
						instructions := b["instructions"].(string)
						if strings.Contains(instructions, webSearchInstructions) != (enabled && !silent) {
							t.Error("wrong web capability instructions")
						}
						fmt.Fprint(w, sse("ok"))
					}))
					defer server.Close()
					a := &AI{cfg: c, http: server.Client(), endpoint: server.URL}
					if _, err := a.reply(context.Background(), []Message{{Role: "user", Content: "question"}}, replyOptions{spontaneous: silent}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
func TestCompletedWebCitations(t *testing.T) {
	text := "Réponse é 😀. citeturn0search0"
	start := len([]rune("Réponse é 😀. "))
	end := len([]rune(text))
	output := []any{
		map[string]any{"type": "web_search_call", "status": "completed"},
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{map[string]any{"type": "url_citation", "start_index": start, "end_index": end, "url": "https://example.org/verified"}}}}},
	}
	done, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": output}})
	rep, err := consumeSSE(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial without citations\"}\n\ndata: " + string(done) + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep.Text, "partial") || strings.Contains(rep.Text, "") || !strings.Contains(rep.Text, "Réponse é 😀. ") || !strings.Contains(rep.Text, "[1]") || len(rep.Sources) != 1 {
		t.Fatalf("bad rendered answer: %+v", rep)
	}
	lines, err := sourcedReplyLines(strings.Repeat(rep.Text, 100), rep.Sources, 3)
	if err != nil || len(lines) != 3 || lines[2] != "[1] https://example.org/verified" {
		t.Fatal(lines, err)
	}
	if _, err = sourcedReplyLines(rep.Text, rep.Sources, 1); err == nil {
		t.Fatal("silently dropped citations")
	}
}
func TestWebCitationsMultipleParts(t *testing.T) {
	raw := `[{"type":"message","content":[{"type":"output_text","text":"One.","annotations":[{"type":"url_citation","start_index":0,"end_index":4,"url":"https://example.org/a"}]},{"type":"output_text","text":"Two.","annotations":[{"type":"url_citation","start_index":0,"end_index":4,"url":"https://example.org/a"},{"type":"url_citation","start_index":4,"end_index":4,"url":"https://example.org/b"}]}]}]`
	var output []responseOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatal(err)
	}
	text, sources, err := renderWebOutput(output)
	if err != nil || text != "One. [1]\nTwo. [1] [2]" || len(sources) != 2 {
		t.Fatal(text, sources, err)
	}
	for _, u := range []string{"javascript:alert(1)", "https://example.org/\nPRIVMSG", "https://u:p@example.org/", "https://example.org/" + strings.Repeat("a", 400)} {
		output[0].Content[0].Annotations[0].URL = u
		if _, _, err := renderWebOutput(output); err == nil {
			t.Fatalf("unsafe URL accepted: %q", u)
		}
	}
}
