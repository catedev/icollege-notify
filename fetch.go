package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// FetchICollege signs in if needed, then reads the course's due items and gradebook
// and returns a Scrape in the same shape the page scraper produced.
func FetchICollege(ctx context.Context, b *Browser, cfg *Config, p Prompter) (*Scrape, error) {
	switch err := b.Login(ctx, cfg, p); err {
	case nil:
	case errBadPassword:
		s := "bad_credentials"
		return &Scrape{ScrapedAt: time.Now(), Error: &s}, nil
	case errMFATimeout:
		s := "login_required"
		return &Scrape{ScrapedAt: time.Now(), Error: &s}, nil
	default:
		return nil, err
	}

	base := fmt.Sprintf("%s/d2l/api/le/1.100/%s", cfg.D2LBase, cfg.CourseOrgUnit)

	var due []d2lDueItem
	next := base + "/content/myItems/due/"
	for next != "" {
		raw, err := b.fetchJSON(next)
		if err != nil {
			return nil, fmt.Errorf("fetching due items: %w", err)
		}
		var page d2lDuePage
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			return nil, fmt.Errorf("parsing due items: %w", err)
		}
		due = append(due, page.Objects...)
		if page.Next != nil {
			next = *page.Next
		} else {
			next = ""
		}
	}

	var objects []d2lGradeObject
	if raw, err := b.fetchJSON(base + "/grades/"); err != nil {
		return nil, fmt.Errorf("fetching grade objects: %w", err)
	} else if err := json.Unmarshal([]byte(raw), &objects); err != nil {
		return nil, fmt.Errorf("parsing grade objects: %w", err)
	}

	var values []d2lGradeValue
	if raw, err := b.fetchJSON(base + "/grades/values/myGradeValues/"); err != nil {
		return nil, fmt.Errorf("fetching grade values: %w", err)
	} else if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("parsing grade values: %w", err)
	}

	b.KeepAlive(cfg) // refresh the idle timer so the session survives to the next run
	if err := b.SaveCookies(b.cookiesPath); err != nil {
		log.Printf("saving session cookies: %v", err)
	}

	return &Scrape{
		ScrapedAt:   time.Now(),
		Course:      cfg.CourseName,
		Assignments: ConvertICollege(due, objects, values, cfg.Location),
	}, nil
}
