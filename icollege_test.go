package main

import (
	"testing"
	"time"
)

func tp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestConvertICollege(t *testing.T) {
	loc := ny(t)
	due := []d2lDueItem{
		// WebAssign homework, linked to a grade object, with a synced score.
		{ItemID: 101, ItemName: "5b Homework: Applications of Functions", DueDate: tp("2026-10-28T03:59:00Z"), ActivityType: 7, ItemURL: "u1"},
		// WebAssign quiz, no score yet.
		{ItemID: 102, ItemName: "Chapter 7 Quiz (7.3 & 7.4)", DueDate: tp("2026-11-12T04:59:00Z"), ActivityType: 7, ItemURL: "u2"},
		// Exempt item: dropped.
		{ItemID: 103, ItemName: "Dropped HW", DueDate: tp("2026-11-01T04:59:00Z"), ActivityType: 7, IsExempt: true},
		// No due date: dropped.
		{ItemID: 104, ItemName: "No due", DueDate: nil, ActivityType: 7},
		// Non-external, non-linked content topic (a reading): dropped.
		{ItemID: 105, ItemName: "How to Use WebAssign", DueDate: tp("2026-09-19T03:59:00Z"), ActivityType: 2},
	}
	objects := []d2lGradeObject{
		{ID: 9001, Name: "5b Homework: Applications of Functions", MaxPoints: 100, CategoryID: 7777, AssociatedTool: &struct {
			ToolID int64 `json:"ToolId"`
		}{ToolID: 390000}},
		{ID: 9002, Name: "Chapter 7 Quiz (7.3 & 7.4)", MaxPoints: 100, CategoryID: 8888, AssociatedTool: &struct {
			ToolID int64 `json:"ToolId"`
		}{ToolID: 390000}},
	}
	score := 100.0
	values := []d2lGradeValue{
		{GradeObjectIdentifier: "7777", GradeObjectName: "Homework", GradeObjectTypeName: "Category"},
		{GradeObjectIdentifier: "8888", GradeObjectName: "Quiz", GradeObjectTypeName: "Category"},
		{GradeObjectIdentifier: "9001", GradeObjectName: "5b Homework", GradeObjectTypeName: "Numeric", PointsNumerator: &score},
	}

	got := ConvertICollege(due, objects, values, loc)
	if len(got) != 2 {
		t.Fatalf("got %d assignments, want 2: %+v", len(got), got)
	}

	hw := got[0]
	if hw.ID != "101" || hw.Category != "Homework" || hw.Score == nil || *hw.Score != 100 {
		t.Errorf("homework mapped wrong: %+v", hw)
	}
	if d, _ := hw.DueTime(loc); d.Format(time.RFC3339) != "2026-10-27T23:59:00-04:00" {
		t.Errorf("homework due = %v, want 2026-10-27T23:59:00-04:00", d)
	}

	quiz := got[1]
	if quiz.ID != "102" || !quiz.Timed || quiz.Score != nil {
		t.Errorf("quiz mapped wrong: %+v", quiz)
	}
	// EST side of the DST change.
	if d, _ := quiz.DueTime(loc); d.Format(time.RFC3339) != "2026-11-11T23:59:00-05:00" {
		t.Errorf("quiz due = %v, want 2026-11-11T23:59:00-05:00", d)
	}
}

func num(f float64) *float64 { return &f }

func TestMergeAssignments(t *testing.T) {
	base := []Assignment{ // iCollege (superset)
		{ID: "ic1", Name: "7.3 Homework: Probability and Odds", MaxScore: 100, Score: nil, Due: "2026-11-02T23:59:00-05:00"},
		{ID: "ic2", Name: "5b Homework: Applications of Functions", MaxScore: 100, Score: num(0)}, // iCollege not synced yet
		{ID: "ic3", Name: "Test 1", MaxScore: 100, Score: num(76)},                                // iCollege-only (no WebAssign)
		{ID: "ic4", Name: "Chapter 7 Quiz (7.3 & 7.4)", MaxScore: 100, Score: nil},
	}
	extra := []Assignment{ // WebAssign
		{ID: "wa2", Name: "5b Homework: Applications of Functions", MaxScore: 100, Score: num(100), Due: "2026-10-27T23:59:00-04:00", Timed: false}, // done on WebAssign
		{ID: "wa3", Name: "Chapter 7 Quiz (7.3 & 7.4)", MaxScore: 100, Score: nil, Timed: true, Due: "2026-11-11T20:00:00-05:00"},                   // per-student timer
		{ID: "wa5", Name: "Getting Started with WebAssign", MaxScore: 100, Score: num(100)},                                                         // WebAssign-only
	}

	got := MergeAssignments(base, extra)
	by := map[string]Assignment{}
	for _, a := range got {
		by[a.Name] = a
	}
	if len(got) != 5 {
		t.Fatalf("got %d merged, want 5: %v", len(got), by)
	}
	// Done on WebAssign => merged counts as completed, even though iCollege had 0.
	if a := by["5b Homework: Applications of Functions"]; !a.Completed() {
		t.Errorf("5b should be completed via WebAssign: %+v", a)
	}
	// Quiz: not done either side, but WebAssign's per-student due time wins.
	q := by["Chapter 7 Quiz (7.3 & 7.4)"]
	if q.Completed() || q.Due != "2026-11-11T20:00:00-05:00" || !q.Timed {
		t.Errorf("quiz merge wrong: %+v", q)
	}
	// iCollege-only and WebAssign-only both survive.
	if _, ok := by["Test 1"]; !ok {
		t.Error("iCollege-only Test 1 dropped")
	}
	if a, ok := by["Getting Started with WebAssign"]; !ok || !a.Completed() {
		t.Error("WebAssign-only item dropped or not complete")
	}
	// Untouched iCollege item keeps its due.
	if by["7.3 Homework: Probability and Odds"].Due != "2026-11-02T23:59:00-05:00" {
		t.Error("iCollege-only due changed")
	}
}

func TestMergeScrapesDegrades(t *testing.T) {
	e := "icollege_fetch_failed"
	ic := &Scrape{Error: &e}
	wa := &Scrape{Assignments: []Assignment{{ID: "1", Name: "HW", MaxScore: 100}}}
	if m := mergeScrapes(ic, wa); m.Error != nil || len(m.Assignments) != 1 {
		t.Errorf("iCollege down should fall back to WebAssign: %+v", m)
	}
	we := "could not read assignments.json: x"
	if m := mergeScrapes(wa, &Scrape{Error: &we}); m.Error != nil || len(m.Assignments) != 1 {
		t.Errorf("WebAssign down should fall back to iCollege: %+v", m)
	}
	if m := mergeScrapes(ic, &Scrape{Error: &we}); m.Error == nil {
		t.Error("both down should surface an error")
	}
}

func TestCompletedTimedVsHomework(t *testing.T) {
	cases := []struct {
		name  string
		score *float64
		timed bool
		want  bool
	}{
		{"timed quiz scored 96.1 → done", num(96.1), true, true},
		{"timed quiz full → done", num(100), true, true},
		{"timed quiz unstarted → not done", nil, true, false},
		{"homework 80 → still remind", num(80), false, false},
		{"homework full → done", num(100), false, true},
		{"homework unstarted → not done", nil, false, false},
	}
	for _, c := range cases {
		a := Assignment{Score: c.score, MaxScore: 100, Timed: c.timed}
		if got := a.Completed(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
