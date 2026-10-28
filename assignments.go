package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Scrape is the assignments.json shape (see SCRAPE_PROMPT.md).
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
func ParseDueRaw(s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(webassignDueLayout, strings.Join(strings.Fields(s), " "), loc)
	if err != nil {
		return time.Time{}, err
	}
	// Go gives unknown zone abbrevs a zero offset; don't silently treat that as UTC
	if _, off := t.Zone(); off == 0 && loc != time.UTC {
		return time.Time{}, fmt.Errorf("unrecognized time zone in %q", s)
	}
	return t, nil
}

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

// timed work (quizzes) is one-shot, so any score counts; homework needs full marks
func (a Assignment) Completed() bool {
	if a.Score == nil || a.MaxScore <= 0 {
		return false
	}
	return *a.Score >= a.MaxScore || a.Timed
}

// done on either side wins, so a stale WebAssign list can't un-complete work
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

// WebAssign's per-student due time wins
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
