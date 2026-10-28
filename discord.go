package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	URL         string       `json:"url,omitempty"`
	Color       int          `json:"color,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Author      *EmbedAuthor `json:"author,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
}

type EmbedAuthor struct {
	Name string `json:"name"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

const maxEmbedsPerMessage = 10

const discordAPI = "https://discord.com/api/v10"

// bot tokens don't expire on their own, so this needs a human
var ErrUnauthorized = errors.New("discord rejected the bot token (401)")

// DM sends over REST only. The bot must share a server with the user, and the user
// must allow DMs from that server.
type DM struct {
	Token   string
	UserID  string
	Client  *http.Client
	BaseURL string // defaults to discordAPI; overridable for tests

	channelID string
}

func (d *DM) channel() (string, error) {
	if d.channelID == "" {
		var ch struct {
			ID string `json:"id"`
		}
		if err := d.call(http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": d.UserID}, &ch); err != nil {
			return "", fmt.Errorf("opening DM channel: %w", err)
		}
		d.channelID = ch.ID
	}
	return d.channelID, nil
}

func (d *DM) Send(embeds []Embed) error {
	ch, err := d.channel()
	if err != nil {
		return err
	}
	msg := map[string]any{"embeds": embeds, "allowed_mentions": map[string]any{"parse": []string{}}}
	if err := d.call(http.MethodPost, "/channels/"+ch+"/messages", msg, nil); err != nil {
		return fmt.Errorf("sending DM: %w", err)
	}
	return nil
}

// retries once on 429
func (d *DM) call(method, path string, payload, out any) error {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	base := d.BaseURL
	if base == "" {
		base = discordAPI
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+d.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DiscordBot (icollege-notify, 1.0)")
		resp, err := d.Client.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			if out != nil {
				return json.Unmarshal(data, out)
			}
			return nil
		case resp.StatusCode == http.StatusUnauthorized:
			return ErrUnauthorized
		case resp.StatusCode == http.StatusTooManyRequests && attempt == 0:
			time.Sleep(retryAfter(resp, data))
			continue
		default:
			return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(data))
		}
	}
}

func retryAfter(resp *http.Response, body []byte) time.Duration {
	var v struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &v) == nil && v.RetryAfter > 0 {
		return time.Duration(v.RetryAfter * float64(time.Second))
	}
	if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil {
		return time.Duration(s * float64(time.Second))
	}
	return 2 * time.Second
}
