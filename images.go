package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/lrstanley/girc"
)

// imageToolName is the function tool the model calls to generate an image.
const imageToolName = "image"

const imageInstructions = `You can generate an image with the %q tool.
Call it with a short, self-contained "prompt" describing the image. Kat generates
it, uploads it and posts the resulting URL to the channel. Use it only when an
image genuinely helps the conversation, at most %d image(s) per answer. The image
is public to everyone who can see the channel.`

func imageTool() any {
	return map[string]any{
		"type":        "function",
		"name":        imageToolName,
		"description": "Generate an image from a text prompt and post it to the channel as a URL.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "A self-contained description of the image to generate."},
			},
			"required":             []string{"prompt"},
			"additionalProperties": false,
		},
	}
}

// imagesToken prefers a dedicated Platform key; the ChatGPT OAuth flow cannot
// generate images, so the shared key is only used with API-key auth.
func (a *AI) imagesToken(ctx context.Context) (string, error) {
	if k := a.cfg.Bot.Images.APIKey; k != "" {
		return k, nil
	}
	if a.cfg.OpenAI.Auth == "api_key" {
		return a.token(ctx)
	}
	return "", errors.New("image generation requires a Platform API key (bot.images.api_key); the ChatGPT OAuth flow does not support image generation")
}

// generateImage requests one image and returns the decoded bytes. The model must
// be an image-generation model; a text model does not emit images.
func (a *AI) generateImage(ctx context.Context, prompt string) ([]byte, error) {
	token, err := a.imagesToken(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"model": a.cfg.Bot.Images.Model, "prompt": prompt, "n": 1}
	if a.cfg.Bot.Images.Size != "" {
		body["size"] = a.cfg.Bot.Images.Size
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.endpoint+"/images/generations", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI images HTTP %d (check images.model and credentials)", res.StatusCode)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 || out.Data[0].B64 == "" {
		return nil, errors.New("OpenAI images response contained no image data")
	}
	data, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil {
		return nil, fmt.Errorf("decoding generated image: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("OpenAI images response contained an empty image")
	}
	return data, nil
}

// filehost uses the configured upload URL, else the soju.im/FILEHOST token.
func (b *Bot) filehost(c *girc.Client) string {
	if b.cfg.Bot.Images.Filehost != "" {
		return b.cfg.Bot.Images.Filehost
	}
	if v, ok := c.GetServerOption("soju.im/FILEHOST"); ok {
		return v
	}
	return ""
}

// uploadImage POSTs the bytes per soju.im/FILEHOST: raw body, Basic auth from the
// SASL PLAIN credentials, 201 + a Location resolved against the upload URL.
func (b *Bot) uploadImage(ctx context.Context, c *girc.Client, data []byte) (string, error) {
	endpoint := b.filehost(c)
	if endpoint == "" {
		return "", errors.New("no file upload host advertised (soju.im/FILEHOST) or configured")
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", errors.New("invalid file upload host URL")
	}
	if b.cfg.IRC.TLS && base.Scheme != "https" {
		return "", errors.New("refusing to upload over plaintext HTTP on a TLS IRC connection")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base.String(), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("Content-Disposition", `attachment; filename="kat.png"`)
	if b.cfg.IRC.SASLUser != "" {
		req.SetBasicAuth(b.cfg.IRC.SASLUser, b.cfg.IRC.SASLPassword)
	}
	res, err := b.ai.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("file upload HTTP %d", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if loc == "" {
		return "", errors.New("file upload response missing Location header")
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return "", errors.New("invalid file upload Location header")
	}
	return base.ResolveReference(ref).String(), nil
}
