package main

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"
)

const (
	colorRed    = 0xE74C3C
	colorOrange = 0xE67E22
	colorYellow = 0xF1C40F
	colorBlue   = 0x3498DB
	colorGreen  = 0x2ECC71
	colorGrey   = 0x95A5A6

	alertScrapeError = "scrape_error"
	alertStale       = "stale"
)

// Notice is one Discord embed plus the state change to record once it is delivered.
type Notice struct {
	Embed Embed
	Apply func(*State)
}

// thresholds must be largest first; returns the smallest crossed one plus all crossed labels
func PickThreshold(remaining time.Duration, thresholds []Threshold) (Threshold, []string, bool) {
	var crossed []string
	var pick Threshold
	for _, t := range thresholds {
		if remaining <= t.D {
			crossed = append(crossed, t.Label)
			pick = t
		}
	}
	return pick, crossed, len(crossed) > 0
}

// bookkeeping is applied to st directly; anything needing a message becomes a Notice applied after delivery
func Plan(now time.Time, sc *Scrape, st *State, cfg *Config) []Notice {
	var notices []Notice

	// Stale data: a broken scrape must not look like "nothing due".
	// TODO: wording assumes the WebAssign/Claude-in-Chrome source, misleading in icollege mode
	maxAge := 3 * cfg.PollInterval
	if age := now.Sub(sc.ScrapedAt); sc.ScrapedAt.IsZero() || age > maxAge {
		if !st.Alerts[alertStale] {
			desc := "assignments.json has no `scraped_at` timestamp."
			if !sc.ScrapedAt.IsZero() {
				desc = fmt.Sprintf("Last successful read of WebAssign was <t:%d:R>. Reminders may be out of date — check that Chrome is open and the Claude extension is connected.", sc.ScrapedAt.Unix())
			}
			notices = append(notices, Notice{
				Embed: Embed{Title: "⚠️ WebAssign data is stale", Description: desc, Color: colorGrey},
				Apply: func(s *State) { s.Alerts[alertStale] = true },
			})
		}
	} else {
		delete(st.Alerts, alertStale)
	}

	if sc.Error != nil {
		if !st.Alerts[alertScrapeError] {
			e := Embed{Title: "⚠️ Couldn't read assignments", Color: colorGrey}
			switch *sc.Error {
			case "login_required":
				e.Description = "Couldn't finish iCollege sign-in (no 2FA response). Reply to the next prompt to resume."
			case "bad_credentials":
				e.Description = "iCollege rejected the stored username/password. Update GSU_USERNAME / GSU_PASSWORD in .env. Sign-in is paused until then."
			default:
				e.Description = fmt.Sprintf("Fetch error: `%s`", *sc.Error)
			}
			notices = append(notices, Notice{Embed: e, Apply: func(s *State) { s.Alerts[alertScrapeError] = true }})
		}
		// Assignment list is empty/untrustworthy: don't announce, remind, or prune.
		return notices
	}
	delete(st.Alerts, alertScrapeError)

	firstRun := !st.Initialized
	current := map[string]bool{}

	list := append([]Assignment(nil), sc.Assignments...)
	type item struct {
		a   Assignment
		due time.Time
	}
	var items []item
	for _, a := range list {
		if a.ID == "" {
			continue
		}
		due, err := a.DueTime(cfg.Location)
		if err != nil {
			log.Printf("skipping %s (%s): %v", a.ID, a.Name, err)
			continue
		}
		items = append(items, item{a, due})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].due.Before(items[j].due) })

	for _, it := range items {
		a, due := it.a, it.due
		id := a.ID
		current[id] = true

		e, seen := st.Assignments[id]
		if !seen {
			e = &Entry{}
			st.Assignments[id] = e
		}
		e.Due = due

		done := a.Completed() && cfg.SkipCompleted

		if !e.Announced {
			if firstRun || !cfg.NotifyNew || done || !due.After(now) {
				e.Announced = true
			} else {
				notices = append(notices, Notice{
					Embed: assignmentEmbed("🆕 New assignment", a, due, colorGreen, sc.Course),
					Apply: func(s *State) { s.Assignments[id].Announced = true },
				})
			}
		}

		if done || !due.After(now) {
			continue
		}
		pick, crossed, ok := PickThreshold(due.Sub(now), cfg.RemindBefore)
		if !ok || e.HasSent(pick.Label) {
			continue
		}
		notices = append(notices, Notice{
			Embed: assignmentEmbed("⏰ Due soon", a, due, urgencyColor(due.Sub(now)), sc.Course),
			Apply: func(s *State) {
				for _, l := range crossed {
					s.Assignments[id].MarkSent(l)
				}
			},
		})
	}

	// Prune entries that have left the page and are well past due.
	for id, e := range st.Assignments {
		if !current[id] && !e.Due.IsZero() && now.Sub(e.Due) > cfg.PruneAfter {
			delete(st.Assignments, id)
		}
	}

	st.Initialized = true
	return notices
}

func urgencyColor(remaining time.Duration) int {
	switch {
	case remaining <= time.Hour:
		return colorRed
	case remaining <= 6*time.Hour:
		return colorOrange
	case remaining <= 24*time.Hour:
		return colorYellow
	default:
		return colorBlue
	}
}

func assignmentEmbed(heading string, a Assignment, due time.Time, color int, course string) Embed {
	score := "not started"
	if a.Score != nil {
		score = fmt.Sprintf("%s / %s", fmtNum(*a.Score), fmtNum(a.MaxScore))
	} else if a.MaxScore > 0 {
		score = fmt.Sprintf("– / %s", fmtNum(a.MaxScore))
	}
	cat := a.Category
	if cat == "" {
		cat = "—"
	}
	if a.Timed {
		cat += " · ⏱ timed"
	}
	e := Embed{
		Title:  truncate(a.Name, 256),
		URL:    a.URL,
		Color:  color,
		Author: &EmbedAuthor{Name: heading},
		Fields: []EmbedField{
			{Name: "Due", Value: fmt.Sprintf("<t:%d:F> (<t:%d:R>)", due.Unix(), due.Unix())},
			{Name: "Category", Value: cat, Inline: true},
			{Name: "Score", Value: score, Inline: true},
		},
		Timestamp: due.UTC().Format(time.RFC3339),
	}
	if course != "" {
		e.Footer = &EmbedFooter{Text: truncate(course, 2048)}
	}
	return e
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
