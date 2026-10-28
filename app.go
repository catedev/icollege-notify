package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// App holds everything one notifier process needs to run a pass: config, the
// Discord REST client, persistent state, and (in iCollege mode) the long-lived
// browser. Its mutex serializes passes and login attempts, since both drive the
// single browser and mutate state.
type App struct {
	cfg     *Config
	dm      *DM
	mu      sync.Mutex
	st      *State
	browser *Browser // nil in file (WebAssign-scrape) mode
	last    *Scrape  // most recent successful fetch, for /assignments
}

func NewApp(cfg *Config, st *State, dm *DM) *App {
	return &App{cfg: cfg, dm: dm, st: st}
}

// ensureBrowser lazily starts the headless Chrome used for iCollege, reusing it
// across passes so the session and trusted-device cookie stay warm.
func (a *App) ensureBrowser(ctx context.Context) {
	if a.cfg.UseICollege && a.browser == nil {
		a.browser = NewBrowser(ctx, a.cfg)
	}
}

func (a *App) Close() {
	if a.browser != nil {
		a.browser.Close()
		a.browser = nil
	}
}

// obtain fetches the current assignment data, applying the bad-credential latch.
func (a *App) obtain(ctx context.Context) *Scrape {
	var sc *Scrape
	switch a.cfg.Source {
	case "webassign":
		sc = a.obtainWebAssign()
	case "icollege":
		sc = a.obtainICollege(ctx)
	default: // "both": iCollege is primary; WebAssign augments completion + due times.
		sc = mergeScrapes(a.obtainICollege(ctx), a.obtainWebAssign())
	}
	if sc.Error == nil {
		a.last = sc
	}
	return sc
}

func (a *App) obtainWebAssign() *Scrape {
	sc, err := LoadScrape(a.cfg.AssignmentsPath)
	if err != nil {
		log.Print(err)
		msg := "could not read assignments.json: " + err.Error()
		return &Scrape{Error: &msg}
	}
	return sc
}

func (a *App) obtainICollege(ctx context.Context) *Scrape {
	credHash := credFingerprint(a.cfg)
	if a.st.BadCredHash == credHash {
		log.Printf("skipping login: credentials unchanged since last rejection")
		s := "bad_credentials"
		return &Scrape{Error: &s}
	}

	a.ensureBrowser(ctx)
	sc, err := FetchICollege(ctx, a.browser, a.cfg, dmPrompter{a.dm})
	if err != nil {
		log.Printf("iCollege fetch: %v", err)
		msg := "icollege_fetch_failed"
		sc = &Scrape{Error: &msg}
	}
	if sc.Error != nil && *sc.Error == "bad_credentials" {
		a.st.BadCredHash = credHash
	} else if sc.Error == nil {
		a.st.BadCredHash = ""
	}
	return sc
}

// mergeScrapes combines iCollege (ic) and WebAssign (wa) results. In "both" mode
// WebAssign is optional augmentation: if it's missing/stale, iCollege is used alone;
// if iCollege failed, WebAssign carries the run; only if both fail is it an error.
func mergeScrapes(ic, wa *Scrape) *Scrape {
	switch {
	case ic.Error != nil && wa.Error != nil:
		return ic // surface the iCollege error (the primary source)
	case ic.Error != nil:
		return wa
	case wa.Error != nil:
		return ic
	}
	at := ic.ScrapedAt
	if wa.ScrapedAt.After(at) {
		at = wa.ScrapedAt
	}
	return &Scrape{
		ScrapedAt:   at,
		Course:      ic.Course,
		Assignments: MergeAssignments(ic.Assignments, wa.Assignments),
	}
}

// Pass runs one fetch → plan → deliver → persist cycle.
func (a *App) Pass(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	sc := a.obtain(ctx)
	notices := Plan(time.Now(), sc, a.st, a.cfg)

	sendErr := a.deliver(notices)
	log.Printf("%d notice(s) planned", len(notices))

	if a.cfg.DryRun {
		return nil // leave state.json untouched
	}
	handleAuthFailure(a.st, sendErr, sc)
	if err := a.st.Save(a.cfg.StatePath); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return sendErr
}

// deliver sends the notices' embeds (≤10 per message) and applies each notice's
// state change only after its message is delivered.
func (a *App) deliver(notices []Notice) error {
	for i := 0; i < len(notices); i += maxEmbedsPerMessage {
		batch := notices[i:min(i+maxEmbedsPerMessage, len(notices))]
		embeds := make([]Embed, len(batch))
		for j, n := range batch {
			embeds[j] = n.Embed
		}
		if a.cfg.DryRun {
			out, _ := json.MarshalIndent(embeds, "", "  ")
			fmt.Println(string(out))
		} else if err := a.dm.Send(embeds); err != nil {
			return err
		}
		for _, n := range batch {
			n.Apply(a.st)
		}
	}
	return nil
}

// ForceLogin re-runs iCollege sign-in on demand (the /login command). It clears the
// bad-credential latch and the MFA-timeout alert first, since the user is explicitly
// retrying, then reports a short human-readable result.
func (a *App) ForceLogin(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.cfg.UseICollege {
		return "Login isn't used in WebAssign-scrape mode."
	}
	a.st.BadCredHash = ""
	delete(a.st.Alerts, alertScrapeError)
	a.ensureBrowser(ctx)

	switch err := a.browser.Login(ctx, a.cfg, dmPrompter{a.dm}); err {
	case nil:
		_ = a.st.Save(a.cfg.StatePath)
		return "✅ Signed in to iCollege. Session is active and this device is remembered."
	case errBadPassword:
		a.st.BadCredHash = credFingerprint(a.cfg)
		_ = a.st.Save(a.cfg.StatePath)
		return "❌ iCollege rejected the stored username/password. Fix GSU_USERNAME / GSU_PASSWORD and restart the service."
	case errMFATimeout:
		return "⌛ No 2FA response in time. Run /login again when you're ready to enter the code."
	default:
		return "⚠️ Login failed: " + err.Error()
	}
}

// credFingerprint is a non-reversible fingerprint of the GSU credentials, used only
// to detect when they change after a rejection. It never stores the password itself.
func credFingerprint(cfg *Config) string {
	sum := sha256.Sum256([]byte(cfg.GSUUsername + "\x00" + cfg.GSUPassword))
	return hex.EncodeToString(sum[:8])
}

const alertDiscordAuth = "discord_auth"

// handleAuthFailure covers the cases where Discord itself can't be used to report a
// problem: a rejected bot token raises one desktop notification until a send succeeds.
func handleAuthFailure(st *State, sendErr error, sc *Scrape) {
	switch {
	case errors.Is(sendErr, ErrUnauthorized):
		if !st.Alerts[alertDiscordAuth] {
			msg := "Discord rejected the bot token. Reset it in the Developer Portal (Bot → Reset Token) and update DISCORD_TOKEN in .env."
			if sc.Error != nil && *sc.Error == "login_required" {
				msg += " Also: iCollege needs a fresh sign-in (/login)."
			}
			desktopNotify("icollege-notify", msg)
			st.Alerts[alertDiscordAuth] = true
		}
	case sendErr == nil:
		delete(st.Alerts, alertDiscordAuth)
	}
}
