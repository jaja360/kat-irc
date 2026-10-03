package main

import (
	"sort"
	"strings"
	"sync"

	"github.com/lrstanley/girc"
)

// memberState is what presence reports for one nick.
type memberState struct {
	account string
	away    string
}

type memberEntry struct {
	nick    string
	account string
	away    string
}

// memberList tracks channel membership plus away/account state from the events
// we observe. It deliberately does not read girc's tracked users: girc mutates
// those fields under its own lock, and reading them from here would be a data
// race. Methods are safe on a nil receiver so presence can be disabled.
type memberList struct {
	mu    sync.Mutex
	chans map[string]map[string]memberState
}

func newMemberList() *memberList {
	return &memberList{chans: map[string]map[string]memberState{}}
}

func (m *memberList) join(ch, nick, account string) {
	if m == nil || nick == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.chans[ch]
	if c == nil {
		c = map[string]memberState{}
		m.chans[ch] = c
	}
	s := c[nick]
	if account != "" {
		s.account = account
	}
	c[nick] = s
}

func (m *memberList) clear(ch string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.chans, ch)
	m.mu.Unlock()
}

func (m *memberList) remove(ch, nick string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if c := m.chans[ch]; c != nil {
		delete(c, nick)
	}
	m.mu.Unlock()
}

func (m *memberList) removeAll(nick string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	for _, c := range m.chans {
		delete(c, nick)
	}
	m.mu.Unlock()
}

func (m *memberList) rename(from, to string) {
	if m == nil || to == "" {
		return
	}
	m.mu.Lock()
	for _, c := range m.chans {
		if s, ok := c[from]; ok {
			delete(c, from)
			c[to] = s
		}
	}
	m.mu.Unlock()
}

// setAway and setAccount apply to every channel the nick appears in, because
// AWAY and ACCOUNT are not channel-scoped.
func (m *memberList) setAway(nick, away string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	for _, c := range m.chans {
		if s, ok := c[nick]; ok {
			s.away = away
			c[nick] = s
		}
	}
	m.mu.Unlock()
}

func (m *memberList) setAccount(nick, account string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	for _, c := range m.chans {
		if s, ok := c[nick]; ok {
			s.account = account
			c[nick] = s
		}
	}
	m.mu.Unlock()
}

// summary returns the tracked members of ch, excluding self, sorted by nick.
func (m *memberList) summary(ch, self string) []memberEntry {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	c := m.chans[ch]
	out := make([]memberEntry, 0, len(c))
	for nick, s := range c {
		if strings.EqualFold(nick, self) {
			continue
		}
		out = append(out, memberEntry{nick: nick, account: s.account, away: s.away})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].nick) < strings.ToLower(out[j].nick) })
	return out
}

// registerPresence keeps the member list current from the events that change
// channel membership or per-user away/account state.
func (b *Bot) registerPresence(c *girc.Client) {
	isSelf := func(e girc.Event) bool {
		return e.Source != nil && strings.EqualFold(e.Source.Name, c.GetNick())
	}
	chanParam := func(e girc.Event) string {
		if len(e.Params) == 0 {
			return ""
		}
		return b.allowedChannel(e.Params[0])
	}
	c.Handlers.Add("353", func(c *girc.Client, e girc.Event) {
		if len(e.Params) < 4 {
			return
		}
		ch := b.allowedChannel(e.Params[2])
		if ch == "" {
			return
		}
		for _, n := range strings.Fields(e.Last()) {
			b.members.join(ch, parseName(n), "")
		}
	})
	c.Handlers.Add(girc.JOIN, func(c *girc.Client, e girc.Event) {
		if e.Source == nil {
			return
		}
		ch := chanParam(e)
		if ch == "" {
			return
		}
		if isSelf(e) {
			b.members.clear(ch)
			return
		}
		account := ""
		if len(e.Params) >= 2 && e.Params[1] != "*" {
			account = e.Params[1]
		}
		b.members.join(ch, e.Source.Name, account)
	})
	c.Handlers.Add(girc.PART, func(c *girc.Client, e girc.Event) {
		if e.Source == nil {
			return
		}
		ch := chanParam(e)
		if ch == "" {
			return
		}
		if isSelf(e) {
			b.members.clear(ch)
			return
		}
		b.members.remove(ch, e.Source.Name)
	})
	c.Handlers.Add(girc.KICK, func(c *girc.Client, e girc.Event) {
		if len(e.Params) < 2 {
			return
		}
		if ch := b.allowedChannel(e.Params[0]); ch != "" {
			b.members.remove(ch, e.Params[1])
		}
	})
	c.Handlers.Add(girc.QUIT, func(c *girc.Client, e girc.Event) {
		if e.Source != nil {
			b.members.removeAll(e.Source.Name)
		}
	})
	c.Handlers.Add(girc.NICK, func(c *girc.Client, e girc.Event) {
		if e.Source != nil && len(e.Params) > 0 {
			b.members.rename(e.Source.Name, e.Last())
		}
	})
	c.Handlers.Add("AWAY", func(c *girc.Client, e girc.Event) {
		if e.Source == nil {
			return
		}
		away := ""
		if len(e.Params) > 0 {
			away = clean(e.Last())
		}
		b.members.setAway(e.Source.Name, away)
	})
	c.Handlers.Add("ACCOUNT", func(c *girc.Client, e girc.Event) {
		if e.Source == nil || len(e.Params) < 1 {
			return
		}
		account := e.Params[0]
		if account == "*" {
			account = ""
		}
		b.members.setAccount(e.Source.Name, account)
	})
}

// parseName strips NAMES prefixes and any userhost part from a name token.
func parseName(n string) string {
	n = strings.TrimLeft(n, "@+%&~")
	if i := strings.IndexByte(n, '!'); i >= 0 {
		n = n[:i]
	}
	return n
}
