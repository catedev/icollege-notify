package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Skipped in -short; needs a browser and network.
func TestBrowserLaunchHelium(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real browser")
	}
	dir, err := os.MkdirTemp("", "helium-test-")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ChromeProfileDir: dir, Headless: true}
	b := NewBrowser(context.Background(), cfg)
	t.Cleanup(func() {
		b.Close()
		time.Sleep(500 * time.Millisecond) // let Helium flush and exit
		os.RemoveAll(dir)
	})

	ctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancel()
	var body string
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://example.com"),
		chromedp.Text("body", &body, chromedp.ByQuery),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "example") && !strings.Contains(body, "documentation") {
		t.Errorf("unexpected body: %q", body)
	}
}
