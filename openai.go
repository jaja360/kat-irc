package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.openai.com/v1"

type AI struct {
	cfg      Config
	http     *http.Client
	endpoint string
}

func newAI(c Config) (*AI, error) {
	a := &AI{cfg: c, http: &http.Client{Timeout: time.Duration(c.OpenAI.TimeoutSeconds) * time.Second}, endpoint: apiBase}
	if c.OpenAI.Auth == "api_key" {
		if c.OpenAI.APIKey == "" {
			return nil, fmt.Errorf("openai.api_key (or OPENAI_API_KEY) is required")
		}
	} else {
		var e error
		_, e = readCredentials(c.OpenAI.CredentialsFile)
		if e != nil {
			return nil, fmt.Errorf("run login first: %w", e)
		}
	}
	return a, nil
}
func (a *AI) token(ctx context.Context) (string, error) {
	if a.cfg.OpenAI.Auth == "api_key" {
		return a.cfg.OpenAI.APIKey, nil
	}
	return oauthToken(ctx, a.cfg.OpenAI.CredentialsFile)
}
func (a *AI) reply(ctx context.Context, messages []Message) (string, error) {
	token, e := a.token(ctx)
	if e != nil {
		return "", e
	}
	body := map[string]any{"model": a.cfg.OpenAI.Model, "instructions": a.cfg.Bot.Persona, "input": messages, "store": false, "stream": true}
	if a.cfg.OpenAI.Auth == "api_key" {
		body["max_output_tokens"] = a.cfg.OpenAI.MaxOutputTokens
	}
	if a.cfg.OpenAI.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": a.cfg.OpenAI.ReasoningEffort}
	}
	b, e := json.Marshal(body)
	if e != nil {
		return "", e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", a.endpoint+"/responses", bytes.NewReader(b))
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	res, e := a.http.Do(req)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI HTTP %d (check credentials, model and quota)", res.StatusCode)
	}
	return consumeSSE(io.LimitReader(res.Body, 8<<20))
}

// Only completed streams are published to IRC. No partial answer on quota/network failure.
func consumeSSE(r io.Reader) (string, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 2<<20)
	var text strings.Builder
	var data []string
	process := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		b := strings.Join(data, "\n")
		data = nil
		if b == "[DONE]" {
			return false, nil
		}
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if e := json.Unmarshal([]byte(b), &ev); e != nil {
			return false, fmt.Errorf("invalid OpenAI stream")
		}
		switch ev.Type {
		case "response.output_text.delta", "response.refusal.delta":
			text.WriteString(ev.Delta)
			if text.Len() > 128000 {
				return false, fmt.Errorf("response exceeds local size limit")
			}
		case "response.completed":
			return true, nil
		case "response.failed", "response.incomplete", "error":
			return false, fmt.Errorf("OpenAI stream ended with %s", ev.Type)
		}
		return false, nil
	}
	for s.Scan() {
		line := s.Text()
		if line == "" {
			done, e := process()
			if e != nil {
				return "", e
			}
			if done {
				out := strings.TrimSpace(text.String())
				if out == "" {
					return "", fmt.Errorf("empty OpenAI response")
				}
				return out, nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if e := s.Err(); e != nil {
		return "", e
	}
	return "", fmt.Errorf("OpenAI stream interrupted before response.completed")
}
func (a *AI) models(ctx context.Context) error {
	token, e := a.token(ctx)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", a.endpoint+"/models", nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := a.http.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("models HTTP %d", res.StatusCode)
	}
	var v struct {
		Models []struct {
			Slug       string `json:"slug"`
			Display    string `json:"display_name"`
			Visibility string `json:"visibility"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&v); e != nil {
		return e
	}
	for _, m := range v.Models {
		if m.Visibility == "list" {
			fmt.Printf("%s\t%s\n", m.Slug, m.Display)
		}
	}
	for _, m := range v.Data {
		fmt.Println(m.ID)
	}
	return nil
}
