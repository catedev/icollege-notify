package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

var (
	errBadPassword = errors.New("GSU rejected the username or password")
	errMFATimeout  = errors.New("no 2FA response in time")
)

// Prompter is how the login flow reaches the (single, configured) user during 2FA —
// one-way: it tells the user, as an embed, the Duo code to enter in Duo Mobile.
type Prompter interface {
	Tell(e Embed) error
}

type dmPrompter struct{ dm *DM }

func (p dmPrompter) Tell(e Embed) error { return p.dm.Send([]Embed{e}) }

// Browser is a headless Chrome with a persistent profile directory, so the GSU
// session and Duo's "remember this device" cookie survive between runs.
type Browser struct {
	alloc       context.Context
	allocCancel context.CancelFunc
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex // serializes chromedp actions on the single target
	cookiesPath string     // where session cookies are persisted between runs
}

func NewBrowser(parent context.Context, cfg *Config) *Browser {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(cfg.ChromeProfileDir),
		chromedp.Flag("disable-gpu", true),
		// Headless Chromium crashes ("context canceled") on heavier pages when the
		// default /dev/shm is small; use /tmp instead. no-sandbox avoids crashes
		// where the user namespace sandbox isn't available for the service user.
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-sandbox", true),
		// Suppress the "didn't shut down correctly" restore bubble/state that a prior
		// crash leaves in a persistent profile and that can wedge the next launch.
		chromedp.Flag("disable-session-crashed-bubble", true),
		chromedp.Flag("hide-crash-restore-bubble", true),
		chromedp.WindowSize(1280, 900),
		// Present headless and headed identically: drop chromedp's default
		// enable-automation switch and the AutomationControlled flag, so
		// navigator.webdriver is undefined in both modes.
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if cfg.Headless {
		// New headless mode renders like a real browser (old headless's redirect/JS
		// quirks broke the SSO flow). The remaining tell is the "HeadlessChrome" UA
		// token, which the IdP blocks; it's normalized to the headed UA after launch.
		opts = append(opts, chromedp.Flag("headless", "new"))
	} else {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	if f := os.Getenv("NOTIFIER_BROWSER_LOG"); f != "" {
		if w, err := os.Create(f); err == nil {
			opts = append(opts, chromedp.CombinedOutput(w))
		}
	}
	// Drive a specific Chromium-based browser (e.g. Helium) when BROWSER_PATH is set
	// or a known binary is found; otherwise chromedp auto-detects Chrome/Chromium.
	if path := resolveBrowserPath(cfg.BrowserPath); path != "" {
		log.Printf("using browser: %s", path)
		opts = append(opts, chromedp.ExecPath(path))
	}
	// A prior run that was killed (not shut down cleanly) leaves Singleton lock files
	// that make Chromium abort with "Failed to create SingletonLock". Since only one
	// notifier uses this profile at a time, clear stale locks before launching.
	for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		_ = os.Remove(filepath.Join(cfg.ChromeProfileDir, name))
	}

	alloc, allocCancel := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancel := chromedp.NewContext(alloc)
	// Start the browser bound to ctx itself. Otherwise the first Run (which boots
	// the browser) would be a per-operation timeout context, and its cancel would
	// tear the browser down, breaking every subsequent action with "context canceled".
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		log.Printf("starting browser: %v", err)
	}
	b := &Browser{alloc: alloc, allocCancel: allocCancel, ctx: ctx, cancel: cancel,
		cookiesPath: filepath.Join(cfg.ChromeProfileDir, "session-cookies.json")}
	if cfg.Headless {
		b.normalizeHeadlessUA()
	}
	// Restore a saved session so routine runs reuse it instead of re-doing SSO.
	if err := b.LoadCookies(b.cookiesPath); err != nil {
		log.Printf("restoring session cookies: %v", err)
	}
	return b
}

// normalizeHeadlessUA makes the headless user-agent identical to the headed one by
// removing only the "HeadlessChrome" token from the browser's own real UA (keeping the
// same Chrome version). The IdP blocks the "HeadlessChrome" token, so without this the
// SSO login never renders. This derives the string at runtime rather than spoofing a
// fixed version, so it stays correct as the browser updates.
func (b *Browser) normalizeHeadlessUA() {
	var ua string
	if err := b.run(10*time.Second, chromedp.Evaluate(`navigator.userAgent`, &ua)); err != nil {
		return
	}
	if !strings.Contains(ua, "HeadlessChrome") {
		return
	}
	fixed := strings.Replace(ua, "HeadlessChrome", "Chrome", 1)
	_ = b.run(10*time.Second, chromedp.ActionFunc(func(ctx context.Context) error {
		return emulation.SetUserAgentOverride(fixed).Do(ctx)
	}))
	log.Printf("headless browser presented as headed")
}

// resolveBrowserPath picks the browser binary chromedp should launch. An explicit
// BROWSER_PATH wins; otherwise it prefers Helium if installed, then falls back to
// chromedp's own auto-detection (returning "").
func resolveBrowserPath(configured string) string {
	if configured != "" {
		return configured
	}
	for _, p := range []string{"helium", "/usr/bin/helium", "/opt/helium/helium"} {
		if full, err := exec.LookPath(p); err == nil {
			return full
		}
	}
	return ""
}

func (b *Browser) Close() {
	b.cancel()
	b.allocCancel()
}

// fetchJSON navigates to a D2L API URL (using the profile's session) and returns
// the response body text. It waits for the body node and retries once, so a
// page-load race can't be mistaken for a lost session (which would force re-login).
func (b *Browser) fetchJSON(url string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(b.ctx, 45*time.Second)
		var body string
		err := chromedp.Run(ctx,
			chromedp.Navigate(url),
			chromedp.WaitReady("body", chromedp.ByQuery),
			chromedp.Text("body", &body, chromedp.ByQuery),
		)
		cancel()
		if err == nil {
			return body, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return "", lastErr
}

// run executes chromedp actions on the browser's own context (b.ctx) with a timeout.
// chromedp.Run requires a context rooted in the chromedp context, so actions must
// never be run on a caller-supplied context such as context.Background().
func (b *Browser) run(timeout time.Duration, actions ...chromedp.Action) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// currentURL reads the current page URL without navigating (safe to poll during MFA).
func (b *Browser) currentURL() string {
	var u string
	_ = b.run(10*time.Second, chromedp.Location(&u))
	return u
}

// navigate goes to url, tolerating the net::ERR_ABORTED that SSO redirect chains
// produce: the initial document is superseded by a redirect before it finishes
// loading, which chromedp.Navigate surfaces as an error even though the flow is fine.
func (b *Browser) navigate(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, 45*time.Second)
	defer cancel()
	err := chromedp.Run(ctx, chromedp.Navigate(url))
	if err != nil && (strings.Contains(err.Error(), "ERR_ABORTED") || strings.Contains(err.Error(), "net::")) {
		return nil
	}
	return err
}

// loggedIn reports whether the profile currently has a usable D2L session.
func (b *Browser) loggedIn(cfg *Config) bool {
	body, err := b.fetchJSON(cfg.D2LBase + "/d2l/api/lp/1.6/users/whoami")
	if err != nil {
		log.Printf("session check failed: %v", err)
		return false
	}
	return strings.Contains(body, `"Identifier"`)
}

// listInputsJS describes the form controls on the page, to diagnose selector drift.
const listInputsJS = `JSON.stringify(Array.from(document.querySelectorAll('input,button'))
  .map(e => ({type: e.type, id: e.id, name: e.name, placeholder: e.placeholder})).slice(0, 30))`

// describePage returns the current URL, title, and a short dump of input fields.
func (b *Browser) describePage() string {
	var loc, title, inputs string
	_ = b.run(10*time.Second,
		chromedp.Location(&loc),
		chromedp.Title(&title),
		chromedp.Evaluate(listInputsJS, &inputs),
	)
	return fmt.Sprintf("url=%q title=%q inputs=%s", loc, title, inputs)
}

// Login establishes a session if one isn't already active. On an active session it
// returns immediately (the common case, thanks to the persistent profile).
func (b *Browser) Login(ctx context.Context, cfg *Config, p Prompter) error {
	if b.loggedIn(cfg) {
		return nil
	}
	log.Printf("no active iCollege session; signing in")

	start := cfg.D2LBase + "/d2l/lp/auth/saml/initiate-login?entityId=" + cfg.IdPEntityID
	if err := b.navigate(start); err != nil {
		return fmt.Errorf("opening login page: %w", err)
	}

	// GSU runs oxAuth (Gluu): JSF field ids contain a colon, which is invalid in a
	// bare #id CSS selector, so match by name attribute instead.
	const (
		userSel   = `input[name="loginForm:username"]`
		passSel   = `input[name="loginForm:password"]`
		submitSel = `input[name="loginForm:loginButton"], button[name="loginForm:loginButton"]`
	)
	// WaitVisible rides out the SSO redirect chain to the final login form.
	if err := b.run(45*time.Second, chromedp.WaitVisible(userSel, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("username field not found: %w; page: %s", err, b.describePage())
	}
	log.Printf("login page: %s", b.currentURL())
	// "Remember me" keeps the GSU session alive longer; set it before submitting.
	_ = b.run(5*time.Second, chromedp.Evaluate(`(()=>{const r=document.getElementById('rememberme');if(r){r.value='true';return true}return false})()`, nil))
	if err := b.run(30*time.Second,
		chromedp.SendKeys(userSel, cfg.GSUUsername, chromedp.ByQuery),
		chromedp.SendKeys(passSel, cfg.GSUPassword, chromedp.ByQuery),
		chromedp.Click(submitSel, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("submitting credentials: %w", err)
	}

	// Decide the outcome: staying on the oxAuth login page means the credentials
	// were rejected (latch against retrying); leaving it means MFA or success.
	if b.stuckOnLogin() {
		log.Printf("still on login page after submit: %s", b.describePage())
		return errBadPassword
	}
	if err := b.completeMFA(ctx, cfg, p); err != nil {
		return err
	}
	if !b.loggedIn(cfg) {
		return errors.New("login did not establish a session")
	}
	log.Printf("iCollege sign-in complete (device remembered for future runs)")
	return nil
}

// stuckOnLogin reports whether, several seconds after submitting, the browser is
// still on the oxAuth login page — the signature of rejected credentials.
func (b *Browser) stuckOnLogin() bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var u string
		if err := b.run(10*time.Second, chromedp.Location(&u)); err != nil {
			return false
		}
		if !strings.Contains(u, "/oxauth/login") {
			return false // moved on to MFA or D2L
		}
		time.Sleep(2 * time.Second)
	}
	return true
}

// trustDeviceJS ticks any "remember this device" / "trust this browser" checkbox Duo
// presents, so future runs skip the 2FA prompt. It's a no-op if none is shown.
const trustDeviceJS = `
(() => {
  const want = /remember|trust/i;
  let hit = false;
  for (const el of document.querySelectorAll('input[type=checkbox]')) {
    const label = (el.labels && el.labels[0] && el.labels[0].innerText) || el.name || el.id || '';
    if (want.test(label) && !el.checked) { el.click(); hit = true; }
  }
  return hit;
})()
`

// duoCodeJS reads the verification code Duo shows for "verified push" — the number
// the user types into Duo Mobile to approve. Duo's Universal Prompt usually renders
// it in the top document (GSU's oxAuth hosts it same-origin); if it's in a
// cross-origin iframe this returns "" and we fall back to a plain "approve" message.
const duoCodeJS = `
(() => {
  const grab = doc => {
    const el = doc.querySelector('.verification-code, [class*="verification-code"], .passcode, .code-display');
    if (el && /^\d{1,8}$/.test(el.innerText.trim())) return el.innerText.trim();
    const t = (doc.body && doc.body.innerText) || '';
    const m = t.match(/code[^0-9]{0,30}(\d{3,8})/i) || t.match(/(\d{3,8})[^0-9]{0,30}in\s+Duo/i);
    return m ? m[1] : '';
  };
  let c = grab(document);
  if (!c) for (const f of document.querySelectorAll('iframe')) {
    try { c = grab(f.contentDocument); if (c) break; } catch (e) {}
  }
  return c;
})()
`

// duoTextJS dumps a little page text to the log, to diagnose the Duo prompt shape.
const duoTextJS = `((document.body&&document.body.innerText)||'').replace(/\s+/g,' ').slice(0,300)`

// trustButtonJS clicks the affirmative button on Duo's post-approval "Trust this
// browser?" / "Remember this device" screen (so the session is remembered and the
// flow proceeds), while avoiding the negative "No, other people use this device"
// choice. Returns the clicked label, or "" if no such button is present.
const trustButtonJS = `
(() => {
  const want = /trust|remember/i, skip = /no[,.]|other people|don'?t|not my|someone else/i;
  const find = doc => {
    for (const el of doc.querySelectorAll('button, input[type=submit], input[type=button], a, [role=button]')) {
      const t = ((el.innerText || el.value || el.getAttribute('aria-label') || '') + '').trim();
      if (t && want.test(t) && !skip.test(t)) { el.click(); return t; }
    }
    return '';
  };
  let r = find(document);
  if (!r) for (const f of document.querySelectorAll('iframe')) { try { r = find(f.contentDocument); if (r) break; } catch (e) {} }
  return r;
})()
`

// completeMFA handles Duo "verified push": the page shows a code that the user types
// into Duo Mobile to approve. The bot reads that code and DMs it (so a headless/server
// run works without a screen), ticks "trust this browser", then waits for the Duo page
// to redirect onward — the sign the user approved. The DM is informational; approval
// happens in Duo Mobile, not here.
func (b *Browser) completeMFA(ctx context.Context, cfg *Config, p Prompter) error {
	if b.redirectedOffAuth(5 * time.Second) {
		return nil // "remember this device" cookie carried us straight through
	}
	// Give the Duo prompt a moment to render, then tick trust + read the code.
	time.Sleep(3 * time.Second)
	var trusted bool
	_ = b.run(10*time.Second, chromedp.Evaluate(trustDeviceJS, &trusted))

	var dbg string
	_ = b.run(10*time.Second, chromedp.Evaluate(duoTextJS, &dbg))
	log.Printf("duo prompt: %s", dbg)

	var code string
	_ = b.run(10*time.Second, chromedp.Evaluate(duoCodeJS, &code))
	if code != "" {
		_ = p.Tell(Embed{
			Author:      &EmbedAuthor{Name: "iCollege sign-in"},
			Title:       "🔐 Approve in Duo Mobile",
			Description: "This login needs two-factor approval. Open **Duo Mobile** and enter the code below to approve.",
			Color:       colorBlue,
			Fields:      []EmbedField{{Name: "Enter this code in Duo Mobile", Value: "**" + code + "**"}},
			Footer:      &EmbedFooter{Text: "This device will be remembered, so you won't be asked every time."},
		})
		log.Printf("duo verification code: %s (sent via DM; enter it in Duo Mobile)", code)
	} else {
		_ = p.Tell(Embed{
			Author:      &EmbedAuthor{Name: "iCollege sign-in"},
			Title:       "🔐 Approve in Duo Mobile",
			Description: "This login needs two-factor approval. Open **Duo Mobile** and approve the request (or enter the code it shows).",
			Color:       colorBlue,
			Footer:      &EmbedFooter{Text: "This device will be remembered, so you won't be asked every time."},
		})
		log.Printf("duo code not readable from page; asked user to approve in Duo Mobile")
	}

	// Wait (bounded) for the user to approve in Duo Mobile; success = redirect onward.
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if b.waitRedirectOffAuth(waitCtx) {
		return nil
	}
	return errMFATimeout
}

// redirectedOffAuth reports whether, within the window, the page has left the
// oxAuth/Duo flow (the sign of MFA completing). It only reads the URL, never
// navigates, so it won't disturb a Duo prompt the user is interacting with.
func (b *Browser) redirectedOffAuth(within time.Duration) bool {
	ctx, cancel := context.WithTimeout(b.ctx, within)
	defer cancel()
	return b.waitRedirectOffAuth(ctx)
}

func (b *Browser) waitRedirectOffAuth(ctx context.Context) bool {
	for i := 0; ; i++ {
		// After the user approves, Duo shows a "Trust this browser?" screen that must
		// be clicked through; do it here so the flow doesn't stall on it.
		var clicked string
		_ = b.run(10*time.Second, chromedp.Evaluate(trustButtonJS, &clicked))
		if clicked != "" {
			log.Printf("clicked trust-device button: %q", clicked)
		}

		u := b.currentURL()
		if u != "" && !strings.Contains(u, "oxauth") && !strings.Contains(u, "duosecurity") && !strings.Contains(u, "/duo") {
			return true
		}
		// Periodically log the stuck screen so its controls can be identified.
		if clicked == "" && i%5 == 4 {
			var dbg string
			_ = b.run(10*time.Second, chromedp.Evaluate(duoTextJS, &dbg))
			log.Printf("still on auth (%s): %s", u, dbg)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// KeepAlive touches an authenticated endpoint to refresh the D2L idle timer. The
// notifier calls this every run, so frequent polling keeps the session from
// expiring on inactivity between genuine fetches.
func (b *Browser) KeepAlive(cfg *Config) {
	if _, err := b.fetchJSON(cfg.D2LBase + "/d2l/api/lp/1.6/users/whoami"); err != nil {
		log.Printf("keep-alive ping failed: %v", err)
	}
}
