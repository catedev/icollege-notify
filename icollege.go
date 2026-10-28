package main

import (
	"strconv"
	"strings"
	"time"
)

// Brightspace (D2L) API shapes, trimmed to the fields we use.

type d2lDueItem struct {
	ItemID       int64      `json:"ItemId"`
	ItemName     string     `json:"ItemName"`
	ItemURL      string     `json:"ItemUrl"`
	DueDate      *time.Time `json:"DueDate"`
	ActivityType int        `json:"ActivityType"`
	IsExempt     bool       `json:"IsExempt"`
}

type d2lDuePage struct {
	Objects []d2lDueItem `json:"Objects"`
	Next    *string      `json:"Next"`
}

type d2lGradeObject struct {
	ID             int64   `json:"Id"`
	Name           string  `json:"Name"`
	MaxPoints      float64 `json:"MaxPoints"`
	CategoryID     int64   `json:"CategoryId"`
	AssociatedTool *struct {
		ToolID int64 `json:"ToolId"`
	} `json:"AssociatedTool"`
}

type d2lGradeValue struct {
	GradeObjectIdentifier string   `json:"GradeObjectIdentifier"`
	GradeObjectName       string   `json:"GradeObjectName"`
	GradeObjectTypeName   string   `json:"GradeObjectTypeName"`
	PointsNumerator       *float64 `json:"PointsNumerator"`
}

// d2lActivityExternal is the ActivityType D2L reports for external-tool (LTI) topics,
// which is how WebAssign assignments are linked into the course.
const d2lActivityExternal = 7

// ConvertICollege turns one course's due items + gradebook into the notifier's
// Assignment list. Items are WebAssign work when they're external-tool topics or
// match a tool-linked grade item by name. Scores come from synced grade values.
func ConvertICollege(due []d2lDueItem, objects []d2lGradeObject, values []d2lGradeValue, loc *time.Location) []Assignment {
	objByName := map[string]d2lGradeObject{}
	for _, o := range objects {
		if o.AssociatedTool != nil {
			objByName[norm(o.Name)] = o
		}
	}
	valueByID := map[string]d2lGradeValue{}
	categoryName := map[string]string{}
	for _, v := range values {
		valueByID[v.GradeObjectIdentifier] = v
		if v.GradeObjectTypeName == "Category" {
			categoryName[v.GradeObjectIdentifier] = v.GradeObjectName
		}
	}

	var out []Assignment
	for _, it := range due {
		obj, linked := objByName[norm(it.ItemName)]
		if it.DueDate == nil || it.IsExempt || (!linked && it.ActivityType != d2lActivityExternal) {
			continue
		}
		dueLocal := it.DueDate.In(loc)
		// TODO: only "quiz" in the name counts as timed; exams/tests slip through
		a := Assignment{
			ID:       strconv.FormatInt(it.ItemID, 10),
			Name:     it.ItemName,
			Timed:    strings.Contains(strings.ToLower(it.ItemName), "quiz"),
			DueRaw:   dueLocal.Format(webassignDueLayout),
			Due:      dueLocal.Format(time.RFC3339),
			MaxScore: 100,
			URL:      it.ItemURL,
		}
		if linked {
			if obj.MaxPoints > 0 {
				a.MaxScore = obj.MaxPoints
			}
			a.Category = categoryName[strconv.FormatInt(obj.CategoryID, 10)]
			if v, ok := valueByID[strconv.FormatInt(obj.ID, 10)]; ok && v.PointsNumerator != nil {
				s := *v.PointsNumerator
				a.Score = &s
			}
		}
		out = append(out, a)
	}
	return out
}

func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
