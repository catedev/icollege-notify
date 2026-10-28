package main

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// Chromium does not write session cookies (those without an expiry) to disk, so
// D2L's auth cookie is lost when the browser restarts and every run would re-do SSO.
// SaveCookies/LoadCookies persist all cookies to a file and restore them, giving
// session cookies a bounded lifetime so the signed-in session is reused across runs
// until it genuinely expires server-side.

const sessionCookieTTL = 12 * time.Hour

// SaveCookies writes the browser's current cookies to path.
func (b *Browser) SaveCookies(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, 20*time.Second)
	defer cancel()
	var cookies []*network.Cookie
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = storage.GetCookies().Do(ctx)
		return err
	})); err != nil {
		return err
	}
	data, err := json.Marshal(cookies)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadCookies restores cookies saved earlier. Session cookies (no expiry) are given
// a bounded expiry so they survive this launch. Missing file is not an error.
func (b *Browser) LoadCookies(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cookies []*network.Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, 20*time.Second)
	defer cancel()
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range cookies {
			exp := cdp.TimeSinceEpoch(time.Now().Add(sessionCookieTTL))
			if c.Expires > 0 {
				exp = cdp.TimeSinceEpoch(time.Unix(int64(c.Expires), 0))
			}
			p := network.SetCookie(c.Name, c.Value).
				WithDomain(c.Domain).
				WithPath(c.Path).
				WithHTTPOnly(c.HTTPOnly).
				WithSecure(c.Secure).
				WithSameSite(c.SameSite).
				WithExpires(&exp)
			_ = p.Do(ctx) // best-effort per cookie
		}
		return nil
	}))
}
