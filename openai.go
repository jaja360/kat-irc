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

// reactToolName is the function tool the model calls to add a reaction.
const reactToolName = "react"

// maxReactionsPerTurn caps reactions emitted in one triggered answer.
const maxReactionsPerTurn = 2

const reactInstructions = `You can react to messages with an emoji using the %q tool.

Every message coming from IRC is prefixed with [msgid:<id>]. To react, call %q with
"msgid" set to the exact id from that prefix (any message shown above, not only the
most recent one) and "emoji" set to a single emoji.

Rules:
- Only use msgid values you actually saw in the conversation.
- Never react to your own messages.
- At most %d reaction(s) per answer, and only when it genuinely fits.
- Match the tone: never react with laughter to criticism or bad news aimed at you.
- Reacting is optional: if nothing is worth a reaction, do not call the tool.
- The reason is a private one-sentence note; it is never sent to IRC.`

const spontaneousOnlyInstructions = `This is a silent reaction check for the latest message. Reply with exactly one emoji to
react to it, or reply exactly NONE if it does not deserve a reaction. Output nothing else.
Match the message's tone: congratulate praise, show warmth for kindness, amusement for
jokes, and use 🤔 when the message is confusing or unclear. If the message criticizes or
mocks you, pick a sheepish, embarrassed or sad emoji, never a laughing one. Skip purely
logistical messages.`

// presenceInstructions heads the live channel-membership snapshot.
const presenceInstructions = `The IRC channel membership below is live state gathered from the server, not chat.
Users marked "away" may not read or answer promptly; prefer addressing members who are
present. The snapshot can be incomplete or slightly stale.`

const replyInstructions = `If your answer is aimed at one person's message rather than the whole channel, start it with
a reply marker: [[reply:<msgid>]] followed by the answer, using the id from that message's
[msgid:<id>] prefix. Otherwise start directly with your answer. The marker only sets the
thread target; it is not a substitute for the answer.`

// redactToolName is the function tool the model calls to retract its own message.
const redactToolName = "redact"

const redactInstructions = `You can retract one of your own recent messages with the %q tool.
Use it only when something you said should be removed, for example it was wrong,
unwanted, or someone asked you to take it back. Redaction is permanent and visible
to the channel. Set "msgid" to the id of one of your own messages listed below, and
give a short "reason". At most one message per answer.`

func redactTool() any {
	return map[string]any{
		"type":        "function",
		"name":        redactToolName,
		"description": "Retract one of your own earlier messages.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msgid":  map[string]any{"type": "string", "description": "The [msgid:<id>] of one of your own messages."},
				"reason": map[string]any{"type": "string", "description": "Short reason, shown with the redaction."},
			},
			"required":             []string{"msgid"},
			"additionalProperties": false,
		},
	}
}

type ToolCall struct {
	Name      string
	CallID    string
	Arguments string
}
type Reply struct {
	Text  string
	Calls []ToolCall
}
type replyOptions struct {
	// spontaneous marks a "react or stay silent" evaluation: no text, and an
	// empty result is a normal outcome.
	spontaneous bool
	presence    string
	// redact offers the redact tool; ownMessages lists the messages it may target.
	redact      bool
	ownMessages string
}

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

// instructions is the persona plus the enabled tool contracts and presence.
func (a *AI) instructions(opts replyOptions) string {
	s := a.cfg.Bot.Persona
	if a.cfg.Bot.Reactions.Enabled {
		if opts.spontaneous {
			s += "\n\n" + spontaneousOnlyInstructions
		} else {
			s += "\n\n" + fmt.Sprintf(reactInstructions, reactToolName, reactToolName, maxReactionsPerTurn)
		}
	}
	if a.cfg.Bot.Images.Enabled && !opts.spontaneous {
		max := a.cfg.Bot.Images.MaxPerReply
		if max < 1 {
			max = 1
		}
		s += "\n\n" + fmt.Sprintf(imageInstructions, imageToolName, max)
	}
	if a.cfg.Bot.ReplyThreading == "model" && !opts.spontaneous {
		s += "\n\n" + replyInstructions
	}
	if opts.redact {
		s += "\n\n" + fmt.Sprintf(redactInstructions, redactToolName)
		if opts.ownMessages != "" {
			s += "\nYour recent messages:\n" + opts.ownMessages
		}
	}
	if opts.presence != "" {
		s += "\n\n" + presenceInstructions + "\n" + opts.presence
	}
	return s
}

// wireInput exposes message ids to the model so it can react to any of the
// preceding messages, not only the last one.
func (a *AI) wireInput(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.MsgID != "" && m.Role == "user" {
			out = append(out, Message{Role: m.Role, Content: "[msgid:" + m.MsgID + "] " + m.Content})
			continue
		}
		out = append(out, m)
	}
	return out
}
func (a *AI) tools(opts replyOptions) []any {
	var out []any
	// A silent check returns an emoji as text, not a tool call.
	if a.cfg.Bot.Reactions.Enabled && !opts.spontaneous {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        reactToolName,
			"description": "Add an emoji reaction to one message of the conversation.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"emoji":  map[string]any{"type": "string", "description": "A single emoji, e.g. \"\U0001F60F\"."},
					"msgid":  map[string]any{"type": "string", "description": "The id shown in the [msgid:<id>] prefix of the target message."},
					"reason": map[string]any{"type": "string", "description": "Short private justification (not sent to IRC)."},
				},
				"required":             []string{"emoji", "msgid"},
				"additionalProperties": false,
			},
		})
	}
	// Image generation and reply threading are not offered on a silent check.
	if a.cfg.Bot.Images.Enabled && !opts.spontaneous {
		out = append(out, imageTool())
	}
	if opts.redact {
		out = append(out, redactTool())
	}
	return out
}

func (a *AI) reply(ctx context.Context, messages []Message, opts replyOptions) (Reply, error) {
	token, e := a.token(ctx)
	if e != nil {
		return Reply{}, e
	}
	body := map[string]any{"model": a.cfg.OpenAI.Model, "instructions": a.instructions(opts), "input": a.wireInput(messages), "store": false, "stream": true}
	if a.cfg.OpenAI.Auth == "api_key" {
		body["max_output_tokens"] = a.cfg.OpenAI.MaxOutputTokens
	}
	if a.cfg.OpenAI.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": a.cfg.OpenAI.ReasoningEffort}
	}
	if tools := a.tools(opts); len(tools) > 0 {
		body["tools"] = tools
	}
	b, e := json.Marshal(body)
	if e != nil {
		return Reply{}, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", a.endpoint+"/responses", bytes.NewReader(b))
	if e != nil {
		return Reply{}, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	res, e := a.http.Do(req)
	if e != nil {
		return Reply{}, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return Reply{}, fmt.Errorf("OpenAI HTTP %d (check credentials, model and quota)", res.StatusCode)
	}
	rep, e := consumeSSE(io.LimitReader(res.Body, 8<<20))
	if e != nil {
		return Reply{}, e
	}
	// A silent spontaneous turn is a valid outcome; an empty answer is not.
	if rep.Text == "" && len(rep.Calls) == 0 && !opts.spontaneous {
		return Reply{}, fmt.Errorf("empty OpenAI response")
	}
	return rep, nil
}

// Only completed streams are published to IRC. No partial answer on quota/network failure.
func consumeSSE(r io.Reader) (Reply, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 2<<20)
	var text strings.Builder
	var data []string

	type call struct {
		name   string
		callID string
		// final holds the complete arguments from the terminal item event;
		// partial accumulates streaming deltas as a safety net.
		final   string
		partial strings.Builder
	}
	// Responses API events reference a function call either by item id
	// (output_item.*) or by item_id (function_call_arguments.*), both of which
	// carry the same value.
	calls := map[string]*call{}
	var order []string
	get := func(key string) *call {
		if key == "" {
			key = "?"
		}
		c, ok := calls[key]
		if !ok {
			c = &call{}
			calls[key] = c
			order = append(order, key)
		}
		return c
	}

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
			Type      string `json:"type"`
			Delta     string `json:"delta"`
			ItemID    string `json:"item_id"`
			Arguments string `json:"arguments"`
			Item      *struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
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
		case "response.output_item.added", "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				break
			}
			key := ev.Item.ID
			if key == "" {
				key = ev.Item.CallID
			}
			c := get(key)
			if c.name == "" {
				c.name = ev.Item.Name
			}
			if c.callID == "" {
				c.callID = ev.Item.CallID
			}
			if ev.Type == "response.output_item.done" && ev.Item.Arguments != "" {
				c.final = ev.Item.Arguments
			}
		case "response.function_call_arguments.delta":
			c := get(ev.ItemID)
			c.partial.WriteString(ev.Delta)
			if c.partial.Len() > 16<<10 {
				return false, fmt.Errorf("tool call arguments too large")
			}
		case "response.function_call_arguments.done":
			c := get(ev.ItemID)
			c.partial.Reset()
			c.final = ev.Arguments
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
				return Reply{}, e
			}
			if done {
				out := Reply{Text: strings.TrimSpace(text.String())}
				for _, key := range order {
					c := calls[key]
					if c == nil || c.name == "" {
						continue
					}
					args := c.final
					if args == "" {
						args = c.partial.String()
					}
					out.Calls = append(out.Calls, ToolCall{Name: c.name, CallID: c.callID, Arguments: args})
				}
				return out, nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if e := s.Err(); e != nil {
		return Reply{}, e
	}
	return Reply{}, fmt.Errorf("OpenAI stream interrupted before response.completed")
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
