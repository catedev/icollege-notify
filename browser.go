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

// Prompter DMs the user the Duo code during 2FA (one-way).
type Prompter interface {
	Tell(e Embed) error
}

type dmPrompter struct{ dm *DM }

func (p dmPrompter) Tell(e Embed) error { return p.dm.Send([]Embed{e}) }

// persistent profile so the GSU session and Duo's remember-device cookie survive restarts
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
		// small /dev/shm crashes headless on heavy pages; no-sandbox because the service
		// user may not have the userns sandbox
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-sandbox", true),
		// a crashed run's restore state can wedge the next launch
		chromedp.Flag("disable-session-crashed-bubble", true),
		chromedp.Flag("hide-crash-restore-bubble", true),
		chromedp.WindowSize(1280, 900),
		// hide navigator.webdriver so headless and headed look the same
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if cfg.Headless {
		// old headless broke the SSO redirects; the HeadlessChrome UA token is stripped after launch
		opts = append(opts, chromedp.Flag("headless", "new"))
	} else {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	if f := os.Getenv("NOTIFIER_BROWSER_LOG"); f != "" {
		if w, err := os.Create(f); err == nil {
			opts = append(opts, chromedp.CombinedOutput(w))
		}
	}
	if path := resolveBrowserPath(cfg.BrowserPath); path != "" {
		log.Printf("using browser: %s", path)
		opts = append(opts, chromedp.ExecPath(path))
	}
	// a killed run leaves stale Singleton locks and Chromium refuses to start
	for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		_ = os.Remove(filepath.Join(cfg.ChromeProfileDir, name))
	}

	alloc, allocCancel := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancel := chromedp.NewContext(alloc)
	// boot on the long-lived ctx; a timeout ctx here kills the browser on cancel
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		log.Printf("starting browser: %v", err)
	}
	b := &Browser{alloc: alloc, allocCancel: allocCancel, ctx: ctx, cancel: cancel,
		cookiesPath: filepath.Join(cfg.ChromeProfileDir, "session-cookies.json")}
	if cfg.Headless {
		b.normalizeHeadlessUA()
	}
	if err := b.LoadCookies(b.cookiesPath); err != nil {
		log.Printf("restoring session cookies: %v", err)
	}
	return b
}

// the IdP blocks "HeadlessChrome" in the UA; strip just that token from the real one
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

// BROWSER_PATH, then Helium if installed, else "" for chromedp's auto-detect
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

// retries once so a page-load race isn't mistaken for a lost session
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

// must run on b.ctx: chromedp needs a context rooted in its own
func (b *Browser) run(timeout time.Duration, actions ...chromedp.Action) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// doesn't navigate, so it's safe to poll during MFA
func (b *Browser) currentURL() string {
	var u string
	_ = b.run(10*time.Second, chromedp.Location(&u))
	return u
}

// SSO redirects abort the first document, which chromedp reports as an error
func (b *Browser) navigate(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, 45*time.Second)
	defer cancel()
	err := chromedp.Run(ctx, chromedp.Navigate(url))
	// TODO: "net::" also swallows real failures (DNS, offline); they only surface later
	// as the WaitVisible timeout in Login. Narrow this to ERR_ABORTED if that's enough.
	if err != nil && strings.Contains(err.Error(), "net::") {
		return nil
	}
	return err
}

const whoamiPath = "/d2l/api/lp/1.6/users/whoami"

func (b *Browser) loggedIn(cfg *Config) bool {
	body, err := b.fetchJSON(cfg.D2LBase + whoamiPath)
	if err != nil {
		log.Printf("session check failed: %v", err)
		return false
	}
	return strings.Contains(body, `"Identifier"`)
}

// for diagnosing selector drift
const listInputsJS = `JSON.stringify(Array.from(document.querySelectorAll('input,button'))
  .map(e => ({type: e.type, id: e.id, name: e.name, placeholder: e.placeholder})).slice(0, 30))`

func (b *Browser) describePage() string {
	var loc, title, inputs string
	_ = b.run(10*time.Second,
		chromedp.Location(&loc),
		chromedp.Title(&title),
		chromedp.Evaluate(listInputsJS, &inputs),
	)
	return fmt.Sprintf("url=%q title=%q inputs=%s", loc, title, inputs)
}

func (b *Browser) Login(ctx context.Context, cfg *Config, p Prompter) error {
	if b.loggedIn(cfg) {
		return nil
	}
	log.Printf("no active iCollege session; signing in")

	start := cfg.D2LBase + "/d2l/lp/auth/saml/initiate-login?entityId=" + cfg.IdPEntityID
	if err := b.navigate(start); err != nil {
		return fmt.Errorf("opening login page: %w", err)
	}

	// oxAuth's JSF ids contain colons, so select by name
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

	// still on the login page = bad creds (latch so we don't retry); otherwise MFA or done
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

// Duo's verified-push number; "" if it's in a cross-origin iframe
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

const duoTextJS = `((document.body&&document.body.innerText)||'').replace(/\s+/g,' ').slice(0,300)`

// clicks yes on Duo's "Trust this browser?" screen, avoiding "No, other people use this device"
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

// the code is DMed so a headless run works; approval itself happens in Duo Mobile
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
	e := Embed{
		Author:      &EmbedAuthor{Name: "iCollege sign-in"},
		Title:       "🔐 Approve in Duo Mobile",
		Description: "This login needs two-factor approval. Open **Duo Mobile** and approve the request (or enter the code it shows).",
		Color:       colorBlue,
		Footer:      &EmbedFooter{Text: "This device will be remembered, so you won't be asked every time."},
	}
	if code != "" {
		e.Description = "This login needs two-factor approval. Open **Duo Mobile** and enter the code below to approve."
		e.Fields = []EmbedField{{Name: "Enter this code in Duo Mobile", Value: "**" + code + "**"}}
		log.Printf("duo verification code: %s (sent via DM; enter it in Duo Mobile)", code)
	} else {
		log.Printf("duo code not readable from page; asked user to approve in Duo Mobile")
	}
	_ = p.Tell(e)

	// Wait (bounded) for the user to approve in Duo Mobile; success = redirect onward.
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if b.waitRedirectOffAuth(waitCtx) {
		return nil
	}
	return errMFATimeout
}

// only reads the URL so it won't disturb the Duo prompt
func (b *Browser) redirectedOffAuth(within time.Duration) bool {
	ctx, cancel := context.WithTimeout(b.ctx, within)
	defer cancel()
	return b.waitRedirectOffAuth(ctx)
}

func (b *Browser) waitRedirectOffAuth(ctx context.Context) bool {
	for i := 0; ; i++ {
		// Duo's post-approval "Trust this browser?" screen stalls the flow until clicked
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

// KeepAlive refreshes the D2L idle timer between fetches.
func (b *Browser) KeepAlive(cfg *Config) {
	if _, err := b.fetchJSON(cfg.D2LBase + whoamiPath); err != nil {
		log.Printf("keep-alive ping failed: %v", err)
	}
}
