package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lrstanley/girc"
)

type Config struct {
	IRC struct {
		Server             string   `json:"server"`
		Port               int      `json:"port"`
		Nick               string   `json:"nick"`
		Channels           []string `json:"channels"`
		TLS                bool     `json:"tls"`
		ServerName         string   `json:"server_name"`
		CAFile             string   `json:"ca_file"`
		SASLUser           string   `json:"sasl_user"`
		Password           string   `json:"password"`
		SASLPassword       string   `json:"sasl_password"`
		AllowPlaintextAuth bool     `json:"allow_plaintext_auth"`
	} `json:"irc"`
	OpenAI struct {
		Auth            string `json:"auth"`
		APIKey          string `json:"api_key"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		CredentialsFile string `json:"credentials_file"`
		TimeoutSeconds  int    `json:"timeout_seconds"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	} `json:"openai"`
	Bot struct {
		Persona         string   `json:"persona"`
		PersonaFile     string   `json:"persona_file"`
		MemoryFile      string   `json:"memory_file"`
		HistoryMessages int      `json:"history_messages"`
		HistoryFile     string   `json:"history_file"`
		CooldownSeconds int      `json:"cooldown_seconds"`
		MaxReplyLines   int      `json:"max_reply_lines"`
		AllowedAccounts []string `json:"allowed_accounts"`
	} `json:"bot"`
}

func loadConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, fmt.Errorf("invalid configuration JSON or unknown field")
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one JSON object")
	}
	if c.IRC.Server == "" || c.IRC.Port < 1 || c.IRC.Port > 65535 || !girc.IsValidNick(c.IRC.Nick) || len(c.IRC.Channels) == 0 {
		return c, fmt.Errorf("invalid IRC server, port, nick or channels")
	}
	seen := map[string]bool{}
	for _, ch := range c.IRC.Channels {
		if !girc.IsValidChannel(ch) {
			return c, fmt.Errorf("invalid IRC channel")
		}
		key := girc.ToRFC1459(ch)
		if seen[key] {
			return c, fmt.Errorf("duplicate IRC channel")
		}
		seen[key] = true
	}
	if c.OpenAI.Auth != "api_key" && c.OpenAI.Auth != "chatgpt" {
		return c, fmt.Errorf("openai.auth must be api_key or chatgpt")
	}
	if c.OpenAI.Model == "" {
		return c, fmt.Errorf("openai.model is required (use models command for ChatGPT)")
	}
	if c.OpenAI.TimeoutSeconds < 1 {
		c.OpenAI.TimeoutSeconds = 90
	}
	if c.OpenAI.MaxOutputTokens < 1 {
		c.OpenAI.MaxOutputTokens = 800
	}
	if c.OpenAI.CredentialsFile == "" {
		c.OpenAI.CredentialsFile = "oauth.json"
	}
	// Relative file paths are always relative to the configuration file.
	for _, p := range []*string{&c.OpenAI.CredentialsFile, &c.IRC.CAFile, &c.Bot.PersonaFile, &c.Bot.MemoryFile, &c.Bot.HistoryFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
	}
	if c.OpenAI.APIKey == "" {
		c.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}
	if c.IRC.Password == "" {
		c.IRC.Password = os.Getenv("IRC_PASSWORD")
	}
	if c.IRC.SASLPassword == "" {
		c.IRC.SASLPassword = os.Getenv("IRC_SASL_PASSWORD")
	}
	if c.Bot.HistoryMessages < 1 || c.Bot.HistoryMessages > 200 {
		return c, fmt.Errorf("history_messages must be 1..200")
	}
	if c.Bot.MaxReplyLines < 1 || c.Bot.MaxReplyLines > 20 {
		return c, fmt.Errorf("max_reply_lines must be 1..20")
	}
	if c.Bot.CooldownSeconds < 0 {
		return c, fmt.Errorf("cooldown_seconds must be nonnegative")
	}
	for _, path := range []string{c.Bot.PersonaFile, c.Bot.MemoryFile} {
		if path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return c, err
			}
			c.Bot.Persona += "\n\n" + string(b)
		}
	}
	if strings.TrimSpace(c.Bot.Persona) == "" {
		return c, fmt.Errorf("persona is required")
	}
	if !c.IRC.TLS && !c.IRC.AllowPlaintextAuth && (c.IRC.Password != "" || c.IRC.SASLPassword != "") {
		return c, fmt.Errorf("credentials over plaintext IRC require allow_plaintext_auth=true")
	}
	if (c.IRC.SASLUser == "") != (c.IRC.SASLPassword == "") {
		return c, fmt.Errorf("set both sasl_user and sasl_password")
	}
	return c, nil
}
