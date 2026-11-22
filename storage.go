package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func atomicJSON(path string, v any) error {
	return atomicJSONLimit(path, v, 0)
}

func atomicJSONLimit(path string, v any, maxBytes int) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if maxBytes > 0 && len(b) > maxBytes {
		return fmt.Errorf("JSON file exceeds configured byte limit")
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".kat-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}

type Message struct {
	Images  []ImageDescription `json:"images,omitempty"`
	Role    string             `json:"role"`
	Content string             `json:"content"`
	// MsgID is the IRCv3 msgid of the message, when the server provides one.
	// It lets the model target a specific (possibly older) message to react to.
	MsgID    string   `json:"msgid,omitempty"`
	ReplyIDs []string `json:"reply_ids,omitempty"`
}
type History struct {
	mu       sync.Mutex
	path     string
	limit    int
	channels map[string][]Message
	echoes   map[string][]ownMsg
}

func newHistory(path string, limit int) (*History, error) {
	h := &History{path: path, limit: limit, channels: map[string][]Message{}}
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if e == nil {
			if e = json.Unmarshal(b, &h.channels); e != nil {
				return nil, e
			}
		}
	}
	if h.channels == nil {
		h.channels = map[string][]Message{}
	}
	for k, v := range h.channels {
		if len(v) > limit {
			h.channels[k] = v[len(v)-limit:]
		}
	}
	return h, nil
}
func (h *History) add(ch string, m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m.Role == "assistant" {
		remaining := []ownMsg{}
		for _, echo := range h.echoes[ch] {
			if ownEchoMatches(m, echo.text) {
				m = withReplyID(m, echo.id)
			} else {
				remaining = append(remaining, echo)
			}
		}
		if h.echoes != nil {
			h.echoes[ch] = remaining
		}
	}
	v := append(h.channels[ch], m)
	if len(v) > h.limit {
		v = v[len(v)-h.limit:]
	}
	h.channels[ch] = v
}
func (h *History) snapshot(ch string) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.channels[ch]...)
}

// hasMsgID reports whether id belongs to a message currently kept for ch. It
// guards against the model inventing or reusing a msgid from another channel.
func (h *History) hasMsgID(ch, id string) bool {
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.channels[ch] {
		if m.hasMsgID(id) {
			return true
		}
	}
	return false
}
func (h *History) save() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.path == "" {
		return nil
	}
	return atomicJSON(h.path, h.channels)
}

// describeImage replaces message values rather than mutating shared slices:
// concurrent request snapshots remain immutable. No binary data is persisted.
func (h *History) describeImage(ch, raw, description string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.channels[ch]) - 1; i >= 0; i-- {
		m := h.channels[ch][i]
		if (m.Role != "user" && m.Role != "assistant") || !strings.Contains(m.Content, raw) {
			continue
		}
		for _, d := range m.Images {
			if d.URL == raw {
				return false
			}
		}
		m.Images = append(append([]ImageDescription(nil), m.Images...), ImageDescription{URL: raw, Description: description})
		// At most four descriptions can be attached to any retained message.
		if len(m.Images) > 4 {
			m.Images = m.Images[len(m.Images)-4:]
		}
		h.channels[ch][i] = m
		return true
	}
	return false
}

func (m Message) hasMsgID(id string) bool {
	if id == "" {
		return false
	}
	if m.MsgID == id {
		return true
	}
	for _, alias := range m.ReplyIDs {
		if alias == id {
			return true
		}
	}
	return false
}
func withReplyID(m Message, id string) Message {
	if m.hasMsgID(id) {
		return m
	}
	m.ReplyIDs = append(append([]string(nil), m.ReplyIDs...), id)
	if len(m.ReplyIDs) > 64 {
		m.ReplyIDs = m.ReplyIDs[len(m.ReplyIDs)-64:]
	}
	return m
}
func ownEchoMatches(m Message, text string) bool {
	// Match complete wire components to the logical answer. URL-only components
	// are common for image replies. Never bind an id across channels or roles.
	return m.Role == "assistant" && strings.TrimSpace(text) != "" && strings.Contains(m.Content, text)
}
func (h *History) ownEcho(ch, id, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.channels[ch]) - 1; i >= 0; i-- {
		m := h.channels[ch][i]
		if ownEchoMatches(m, text) {
			h.channels[ch][i] = withReplyID(m, id)
			return
		}
	}
	// Echoes can arrive before sendLogicalAnswer returns and the logical answer
	// is recorded. Keep a small connection-local bridge, never extra history rows.
	if h.echoes == nil {
		h.echoes = map[string][]ownMsg{}
	}
	v := append(h.echoes[ch], ownMsg{id: id, text: text})
	if len(v) > 64 {
		v = v[len(v)-64:]
	}
	h.echoes[ch] = v
}

// Reactions may target observed participants, never the bot's own echo ids.
func (h *History) hasUserMsgID(ch, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.channels[ch] {
		if m.Role == "user" && m.hasMsgID(id) {
			return true
		}
	}
	return false
}
