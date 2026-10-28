package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ny(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func testConfig(t *testing.T) *Config {
	th, err := ParseThresholds("72h,24h,6h,1h")
	if err != nil {
		t.Fatal(err)
	}
	return &Config{
		Location:      ny(t),
		PollInterval:  time.Hour,
		RemindBefore:  th,
		SkipCompleted: true,
		NotifyNew:     true,
		PruneAfter:    7 * 24 * time.Hour,
	}
}

func fixture(t *testing.T) *Scrape {
	sc, err := LoadScrape("testdata/assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestParseDueRaw(t *testing.T) {
	loc := ny(t)
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Tuesday, October 27, 2026 at 11:59 PM EDT", "2026-10-27T23:59:00-04:00", false},
		{"Friday, October 30, 2026 at 11:59 PM EDT", "2026-10-30T23:59:00-04:00", false},
		// DST ends Sunday Nov 1, 2026.
		{"Monday, November 2, 2026 at 11:59 PM EST", "2026-11-02T23:59:00-05:00", false},
		{"Wednesday, November 11, 2026 at 11:59 PM EST", "2026-11-11T23:59:00-05:00", false},
		{"Monday,  November 9, 2026 at 9:05 AM EST", "2026-11-09T09:05:00-05:00", false},
		{"Monday, November 9, 2026 at 9:05 AM XYZ", "", true},
		{"next Tuesday", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDueRaw(tt.in, loc)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s := got.Format(time.RFC3339); s != tt.want {
				t.Errorf("got %s, want %s", s, tt.want)
			}
		})
	}
}

func TestFixtureDueOffsets(t *testing.T) {
	loc := ny(t)
	novStart := time.Date(2026, 11, 2, 0, 0, 0, 0, loc)
	for _, a := range fixture(t).Assignments {
		due, err := a.DueTime(loc)
		if err != nil {
			t.Fatalf("%s: %v", a.ID, err)
		}
		_, off := due.Zone()
		want := -4 * 3600
		if !due.Before(novStart) {
			want = -5 * 3600
		}
		if off != want {
			t.Errorf("%s (%s): offset %d, want %d", a.ID, a.DueRaw, off, want)
		}
	}
}

func TestPickThreshold(t *testing.T) {
	th, _ := ParseThresholds("1h, 72h,6h,24h") // order shouldn't matter
	tests := []struct {
		remaining   time.Duration
		wantLabel   string
		wantCrossed []string
	}{
		{100 * time.Hour, "", nil},
		{72 * time.Hour, "72h", []string{"72h"}},
		{30 * time.Hour, "72h", []string{"72h"}},
		{23 * time.Hour, "24h", []string{"72h", "24h"}},
		{5 * time.Hour, "6h", []string{"72h", "24h", "6h"}},
		{10 * time.Minute, "1h", []string{"72h", "24h", "6h", "1h"}},
	}
	for _, tt := range tests {
		pick, crossed, ok := PickThreshold(tt.remaining, th)
		if ok != (tt.wantLabel != "") || pick.Label != tt.wantLabel || strings.Join(crossed, ",") != strings.Join(tt.wantCrossed, ",") {
			t.Errorf("%v: got %q %v %v, want %q %v", tt.remaining, pick.Label, crossed, ok, tt.wantLabel, tt.wantCrossed)
		}
	}
}

func titles(ns []Notice) map[string]string {
	m := map[string]string{}
	for _, n := range ns {
		if n.Embed.Author != nil {
			m[n.Embed.Title] = n.Embed.Author.Name
		} else {
			m[n.Embed.Title] = ""
		}
	}
	return m
}

func applyAll(st *State, ns []Notice) {
	for _, n := range ns {
		n.Apply(st)
	}
}

func TestPlanFixture(t *testing.T) {
	cfg := testConfig(t)
	sc := fixture(t)
	now := time.Date(2026, 10, 27, 14, 10, 0, 0, cfg.Location) // ~9h50m before the Oct 27 deadlines
	st := NewState()

	ns := Plan(now, sc, st, cfg)
	got := titles(ns)

	// Reminders: open items due within 72h. No "new" announcements on first run.
	// Oct 30 23:59 EDT is ~81h50m away (>72h), so those aren't reminded yet.
	want := map[string]bool{
		"5b Homework: Applications of Functions":           true, // 80/100 still reminded
		"Chapter 5 Quiz (5.1 5.2 5a & 5b)":                 true,
		"Pre-Chapter 5: Linear & Nonlinear Functions Quiz": true,
	}
	if len(got) != len(want) {
		t.Errorf("got %d notices %v, want %d", len(got), got, len(want))
	}
	for title := range want {
		if got[title] != "⏰ Due soon" {
			t.Errorf("missing reminder for %q (got %v)", title, got)
		}
	}
	if _, ok := got["Pre-Chapter 5: Linear & Nonlinear Functions Homewk"]; ok {
		t.Error("completed 10000766 should be skipped")
	}
	if !st.Initialized {
		t.Error("state should be initialized after first run")
	}
	applyAll(st, ns)

	// 9h50m out: 72h and 24h crossed, so both marked; 6h not yet.
	if e := st.Assignments["10000770"]; strings.Join(e.Sent, ",") != "72h,24h" {
		t.Errorf("10000770 sent = %v, want [72h 24h]", e.Sent)
	}

	// Same time again: nothing new.
	if ns := Plan(now, sc, st, cfg); len(ns) != 0 {
		t.Errorf("rerun produced %v", titles(ns))
	}

	// 5h before: 6h threshold fires once for the three open Oct 27 items.
	later := time.Date(2026, 10, 27, 19, 0, 0, 0, cfg.Location)
	sc.ScrapedAt = later
	ns = Plan(later, sc, st, cfg)
	if len(ns) != 3 {
		t.Errorf("at 5h out got %v, want 3 reminders", titles(ns))
	}
	applyAll(st, ns)
	if ns := Plan(later.Add(10*time.Minute), sc, st, cfg); len(ns) != 0 {
		t.Errorf("6h reminders repeated: %v", titles(ns))
	}
}

func TestPlanNewAssignmentAndPrune(t *testing.T) {
	cfg := testConfig(t)
	sc := fixture(t)
	now := time.Date(2026, 10, 27, 14, 10, 0, 0, cfg.Location)
	st := NewState()
	applyAll(st, Plan(now, sc, st, cfg))

	sc.Assignments = append(sc.Assignments, Assignment{
		ID: "10000999", Name: "Chapter 8 Homework", Category: "Homework",
		DueRaw: "Monday, November 30, 2026 at 11:59 PM EST", MaxScore: 100,
	})
	ns := Plan(now, sc, st, cfg)
	if g := titles(ns); len(g) != 1 || g["Chapter 8 Homework"] != "🆕 New assignment" {
		t.Fatalf("got %v, want one new-assignment notice", g)
	}
	applyAll(st, ns)
	if !st.Assignments["10000999"].Announced {
		t.Error("not marked announced")
	}

	// Two weeks after the Nov 11 quiz, with an empty page: everything pruned.
	far := time.Date(2026, 12, 15, 12, 0, 0, 0, cfg.Location)
	empty := &Scrape{ScrapedAt: far}
	Plan(far, empty, st, cfg)
	if len(st.Assignments) != 0 {
		t.Errorf("expected all pruned, %d left", len(st.Assignments))
	}
}

func TestPlanUndeliveredIsRetried(t *testing.T) {
	cfg := testConfig(t)
	sc := fixture(t)
	now := time.Date(2026, 10, 27, 14, 10, 0, 0, cfg.Location)
	st := NewState()
	n1 := Plan(now, sc, st, cfg) // not applied: delivery "failed"
	n2 := Plan(now, sc, st, cfg)
	if len(n1) == 0 || len(n1) != len(n2) {
		t.Errorf("undelivered reminders should be replanned: %d then %d", len(n1), len(n2))
	}
}

func TestPlanLoginRequiredAndStale(t *testing.T) {
	cfg := testConfig(t)
	now := time.Date(2026, 10, 27, 14, 10, 0, 0, cfg.Location)
	st := NewState()
	applyAll(st, Plan(now, fixture(t), st, cfg))
	before := len(st.Assignments)

	errStr := "login_required"
	bad := &Scrape{ScrapedAt: now, Error: &errStr}
	ns := Plan(now, bad, st, cfg)
	if len(ns) != 1 || !strings.Contains(ns[0].Embed.Description, "2FA") {
		t.Fatalf("want one sign-in notice, got %+v", ns)
	}
	applyAll(st, ns)
	if ns := Plan(now, bad, st, cfg); len(ns) != 0 {
		t.Error("login notice repeated")
	}
	if len(st.Assignments) != before {
		t.Error("error scrape must not touch assignment state")
	}

	// Stale: scraped 4h ago with 1h poll interval.
	sc := fixture(t)
	sc.ScrapedAt = now.Add(-4 * time.Hour)
	ns = Plan(now, sc, st, cfg)
	if len(ns) != 1 || !strings.Contains(ns[0].Embed.Title, "stale") {
		t.Fatalf("want one stale notice, got %v", titles(ns))
	}
	if st.Alerts[alertScrapeError] {
		t.Error("successful scrape should clear the login alert")
	}
	applyAll(st, ns)
	if ns := Plan(now, sc, st, cfg); len(ns) != 0 {
		t.Error("stale notice repeated")
	}
	sc.ScrapedAt = now
	Plan(now, sc, st, cfg)
	if st.Alerts[alertStale] {
		t.Error("fresh scrape should clear stale alert")
	}
}

func TestDMSend(t *testing.T) {
	var paths []string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			t.Errorf("bad auth header %q", r.Header.Get("Authorization"))
		}
		paths = append(paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/users/@me/channels":
			if !strings.Contains(string(body), `"recipient_id":"42"`) {
				t.Errorf("bad DM open body %s", body)
			}
			io.WriteString(w, `{"id":"999"}`)
		case "/channels/999/messages":
			json.Unmarshal(body, &sent)
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dm := &DM{Token: "tok", UserID: "42", Client: srv.Client(), BaseURL: srv.URL}
	for i := 0; i < 2; i++ {
		if err := dm.Send([]Embed{{Title: "hi"}}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(paths, " ") != "/users/@me/channels /channels/999/messages /channels/999/messages" {
		t.Errorf("unexpected calls %v (DM channel should be opened once)", paths)
	}
	if embeds, _ := sent["embeds"].([]any); len(embeds) != 1 {
		t.Errorf("bad message body %v", sent)
	}
}

func TestDMUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message": "401: Unauthorized", "code": 0}`)
	}))
	defer srv.Close()
	dm := &DM{Token: "bad", UserID: "42", Client: srv.Client(), BaseURL: srv.URL}
	if err := dm.Send([]Embed{{Title: "hi"}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}
}
