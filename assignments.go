package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Scrape is the assignments.json shape (written per SCRAPE_PROMPT.md, or built from iCollege).
type Scrape struct {
	ScrapedAt   time.Time    `json:"scraped_at"`
	Course      string       `json:"course"`
	Error       *string      `json:"error"`
	Assignments []Assignment `json:"assignments"`
}

type Assignment struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Timed    bool     `json:"timed"`
	DueRaw   string   `json:"due_raw"`
	Due      string   `json:"due"`
	Score    *float64 `json:"score"`
	MaxScore float64  `json:"max_score"`
	URL      string   `json:"url"`
}

const webassignDueLayout = "Monday, January 2, 2006 at 3:04 PM MST"

// ParseDueRaw parses WebAssign's "Monday, November 2, 2026 at 11:59 PM EST" in loc.
// The zone abbreviation is matched against loc, so EDT/EST resolve to -04:00/-05:00.
func ParseDueRaw(s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(webassignDueLayout, strings.Join(strings.Fields(s), " "), loc)
	if err != nil {
		return time.Time{}, err
	}
	// Go accepts unknown abbreviations with a zero offset; reject that instead of
	// silently treating the time as UTC.
	if _, off := t.Zone(); off == 0 && loc != time.UTC {
		return time.Time{}, fmt.Errorf("unrecognized time zone in %q", s)
	}
	return t, nil
}

// DueTime prefers the scraper's normalized "due" and falls back to parsing due_raw.
func (a Assignment) DueTime(loc *time.Location) (time.Time, error) {
	if a.Due != "" {
		if t, err := time.Parse(time.RFC3339, a.Due); err == nil {
			return t, nil
		}
	}
	if a.DueRaw == "" {
		return time.Time{}, fmt.Errorf("assignment %s has no due date", a.ID)
	}
	return ParseDueRaw(a.DueRaw, loc)
}

// Completed reports whether an assignment is done and shouldn't be reminded.
// Full marks always count. A timed assignment (quiz/exam) is one-shot: once it has
// any score it's been taken and can't be improved, so a partial score also counts.
// Non-timed homework with a partial score is NOT done — you can keep improving it
// before the deadline, so it still gets reminders.
func (a Assignment) Completed() bool {
	if a.Score == nil || a.MaxScore <= 0 {
		return false
	}
	return *a.Score >= a.MaxScore || a.Timed
}

// MergeAssignments combines two assignment lists, matching by normalized name.
// base is the primary source (iCollege, the superset); extra (WebAssign) contributes
// per-student due times and possibly fresher completion. An assignment counts as done
// if EITHER source shows it done, so work submitted on either platform is not
// re-reminded. Completion is monotonic, so a stale extra list can't un-complete work.
func MergeAssignments(base, extra []Assignment) []Assignment {
	out := append([]Assignment(nil), base...)
	idx := make(map[string]int, len(out))
	for i, a := range out {
		idx[norm(a.Name)] = i
	}
	for _, e := range extra {
		if i, ok := idx[norm(e.Name)]; ok {
			out[i] = mergeOne(out[i], e)
		} else {
			idx[norm(e.Name)] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// mergeOne combines a base (iCollege) and extra (WebAssign) entry for the same
// assignment. Done-on-either wins; WebAssign's per-student due time is preferred.
func mergeOne(base, extra Assignment) Assignment {
	m := base
	if extra.Due != "" || extra.DueRaw != "" {
		m.Due, m.DueRaw = extra.Due, extra.DueRaw
	}
	m.Timed = base.Timed || extra.Timed
	if m.URL == "" {
		m.URL = extra.URL
	}
	switch {
	case base.Completed() || extra.Completed():
		if m.MaxScore <= 0 {
			m.MaxScore = 100
		}
		done := m.MaxScore
		m.Score = &done // normalize to a completed score
	case base.Score == nil && extra.Score != nil:
		m.Score = extra.Score
		if extra.MaxScore > 0 {
			m.MaxScore = extra.MaxScore
		}
	}
	return m
}

func LoadScrape(path string) (*Scrape, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scrape
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}
