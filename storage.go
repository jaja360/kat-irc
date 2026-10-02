package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
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
	Role    string `json:"role"`
	Content string `json:"content"`
}
type History struct {
	mu       sync.Mutex
	path     string
	limit    int
	channels map[string][]Message
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
func (h *History) save() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.path == "" {
		return nil
	}
	return atomicJSON(h.path, h.channels)
}
