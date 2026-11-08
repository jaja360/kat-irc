package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
		User               string   `json:"user"`
		Channels           []string `json:"channels"`
		TLS                bool     `json:"tls"`
		ServerName         string   `json:"server_name"`
		CAFile             string   `json:"ca_file"`
		SASLUser           string   `json:"sasl_user"`
		Password           string   `json:"password"`
		SASLPassword       string   `json:"sasl_password"`
		AllowPlaintextAuth bool     `json:"allow_plaintext_auth"`
		Multiline          string   `json:"multiline"`
	} `json:"irc"`
	OpenAI struct {
		Auth            string `json:"auth"`
		APIKey          string `json:"api_key"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		WebSearch       bool   `json:"web_search"`
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
		MaxReplyBytes   int      `json:"max_reply_bytes"`
		AllowedAccounts []string `json:"allowed_accounts"`
		Typing          bool     `json:"typing"`
		Presence        bool     `json:"presence"`
		ReplyThreading  string   `json:"reply_threading"`
		Reactions       struct {
			// Enabled offers the "react" tool and handles incoming TAGMSG
			// reactions (context only). Disabled unless explicitly turned on.
			Enabled bool `json:"enabled"`
			// Spontaneous allows a rate-limited reaction-only evaluation of
			// ordinary, non-triggering messages.
			Spontaneous bool `json:"spontaneous"`
			// MinIntervalSeconds bounds the cost of spontaneous evaluations.
			MinIntervalSeconds int `json:"min_interval_seconds"`
		} `json:"reactions"`
		Images struct {
			Enabled        bool   `json:"enabled"`
			Model          string `json:"model"`
			Size           string `json:"size"`
			Filehost       string `json:"filehost"`
			APIKey         string `json:"api_key"`
			MaxPerReply    int    `json:"max_per_reply"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		} `json:"images"`
		Redaction     bool                `json:"redaction"`
		ChannelMemory ChannelMemoryConfig `json:"channel_memory"`
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
	if c.Bot.ChannelMemory.Enabled {
		if c.Bot.ChannelMemory.File == "" {
			c.Bot.ChannelMemory.File = "channel-memory.json"
		}
		if c.Bot.ChannelMemory.MaxBytes == 0 {
			c.Bot.ChannelMemory.MaxBytes = 65536
		}
		if c.Bot.ChannelMemory.BatchMessages == 0 {
			c.Bot.ChannelMemory.BatchMessages = 40
		}
		if c.Bot.ChannelMemory.IntervalSeconds == 0 {
			c.Bot.ChannelMemory.IntervalSeconds = 300
		}
		if c.Bot.ChannelMemory.MaxBytes < 4096 || c.Bot.ChannelMemory.MaxBytes > 1048576 {
			return c, fmt.Errorf("channel_memory.max_bytes must be 4096..1048576")
		}
		if c.Bot.ChannelMemory.BatchMessages < 1 || c.Bot.ChannelMemory.BatchMessages > 200 {
			return c, fmt.Errorf("channel_memory.batch_messages must be 1..200")
		}
		if c.Bot.ChannelMemory.IntervalSeconds < 30 {
			return c, fmt.Errorf("channel_memory.interval_seconds must be at least 30")
		}
		if c.Bot.ChannelMemory.MaxBytes < 1024+len(c.IRC.Channels)*2048 {
			return c, fmt.Errorf("channel_memory.max_bytes is too small for the configured channels")
		}
	}
	// Relative file paths are always relative to the configuration file.
	for _, p := range []*string{&c.OpenAI.CredentialsFile, &c.IRC.CAFile, &c.Bot.PersonaFile, &c.Bot.MemoryFile, &c.Bot.HistoryFile, &c.Bot.ChannelMemory.File} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
	}
	if c.Bot.ChannelMemory.Enabled {
		for _, protected := range []string{path, c.Bot.PersonaFile, c.Bot.MemoryFile, c.Bot.HistoryFile, c.OpenAI.CredentialsFile, c.IRC.CAFile} {
			if protected != "" && sameFilePath(c.Bot.ChannelMemory.File, protected) {
				return c, fmt.Errorf("channel_memory.file must be separate from configuration, credentials, persona, memory and history")
			}
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
	if c.IRC.User == "" {
		c.IRC.User = "kat"
	}
	if !girc.IsValidUser(c.IRC.User) {
		return c, fmt.Errorf("irc.user must be a valid IRC ident")
	}
	if c.Bot.HistoryMessages < 1 || c.Bot.HistoryMessages > 200 {
		return c, fmt.Errorf("history_messages must be 1..200")
	}
	if c.IRC.Multiline == "" {
		c.IRC.Multiline = "auto"
	}
	if c.IRC.Multiline != "auto" && c.IRC.Multiline != "off" {
		return c, fmt.Errorf("irc.multiline must be auto or off")
	}
	if c.Bot.MaxReplyBytes == 0 {
		c.Bot.MaxReplyBytes = 8192
	}
	if c.Bot.MaxReplyBytes < 512 || c.Bot.MaxReplyBytes > 65536 {
		return c, fmt.Errorf("max_reply_bytes must be 512..65536")
	}
	if c.Bot.MaxReplyLines < 0 || c.Bot.MaxReplyLines > 20 {
		return c, fmt.Errorf("max_reply_lines must be 0..20 (legacy budget; 0 uses max_reply_bytes)")
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
	if c.Bot.Reactions.Spontaneous && !c.Bot.Reactions.Enabled {
		return c, fmt.Errorf("reactions.spontaneous requires reactions.enabled")
	}
	if c.Bot.Reactions.MinIntervalSeconds < 0 {
		return c, fmt.Errorf("reactions.min_interval_seconds must be nonnegative")
	}
	if c.Bot.Reactions.Spontaneous && c.Bot.Reactions.MinIntervalSeconds < 15 {
		c.Bot.Reactions.MinIntervalSeconds = 180
	}
	if c.Bot.Images.Enabled {
		if c.Bot.Images.Model == "" {
			return c, fmt.Errorf("images.model is required when images.enabled")
		}
		if c.Bot.Images.Size == "" {
			c.Bot.Images.Size = "1024x1024"
		}
		if c.Bot.Images.MaxPerReply < 1 {
			c.Bot.Images.MaxPerReply = 1
		}
		if c.Bot.Images.MaxPerReply > 2 {
			return c, fmt.Errorf("images.max_per_reply must be 1..2")
		}
		if c.Bot.Images.TimeoutSeconds < 1 {
			c.Bot.Images.TimeoutSeconds = 120
		}
		if c.Bot.Images.Filehost != "" {
			u, err := url.Parse(c.Bot.Images.Filehost)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return c, fmt.Errorf("images.filehost must be an http(s) URL")
			}
		}
		if c.Bot.Images.APIKey == "" {
			c.Bot.Images.APIKey = os.Getenv("OPENAI_IMAGES_API_KEY")
		}
		if c.OpenAI.Auth == "chatgpt" && c.Bot.Images.APIKey == "" {
			return c, fmt.Errorf("images.enabled with openai.auth=chatgpt requires images.api_key (or OPENAI_IMAGES_API_KEY): the ChatGPT OAuth flow does not support image generation")
		}
	}
	switch c.Bot.ReplyThreading {
	case "":
		c.Bot.ReplyThreading = "model"
	case "model", "always", "never":
	default:
		return c, fmt.Errorf("reply_threading must be model, always or never")
	}
	return c, nil
}
