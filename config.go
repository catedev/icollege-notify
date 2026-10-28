package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Threshold is a named reminder offset before the due time, e.g. "24h".
type Threshold struct {
	Label string
	D     time.Duration
}

type Config struct {
	BotToken        string
	DMUserID        string
	GuildID         string
	AssignmentsPath string
	StatePath       string
	Location        *time.Location
	PollInterval    time.Duration
	RemindBefore    []Threshold // sorted largest first
	SkipCompleted   bool
	NotifyNew       bool
	PruneAfter      time.Duration
	DryRun          bool

	// Source is "icollege", "webassign", or "both".
	Source       string
	UseWebAssign bool

	// iCollege (D2L Brightspace) source
	UseICollege      bool
	D2LBase          string
	IdPEntityID      string
	CourseOrgUnit    string
	CourseName       string
	GSUUsername      string
	GSUPassword      string
	ChromeProfileDir string
	BrowserPath      string
	Headless         bool
}

// yamlConfig mirrors config.yaml. Pointers mark "unset" so a false in the file is
// distinguishable from an omitted key (which takes the default).
type yamlConfig struct {
	Discord struct {
		Token    string `yaml:"token"`
		DMUserID string `yaml:"dm_user_id"`
		GuildID  string `yaml:"guild_id"`
	} `yaml:"discord"`

	// Source selects where assignments come from: "icollege", "webassign", or "both".
	Source string `yaml:"source"`

	ICollege struct {
		GSUUsername      string `yaml:"gsu_username"`
		GSUPassword      string `yaml:"gsu_password"`
		CourseOrgUnit    string `yaml:"course_org_unit"`
		CourseName       string `yaml:"course_name"`
		D2LBase          string `yaml:"d2l_base"`
		IdPEntityID      string `yaml:"idp_entity_id"`
		ChromeProfileDir string `yaml:"chrome_profile_dir"`
		BrowserPath      string `yaml:"browser_path"`
		Headless         *bool  `yaml:"headless"`
	} `yaml:"icollege"`

	WebAssign struct {
		AssignmentsPath string `yaml:"assignments_path"`
	} `yaml:"webassign"`

	Reminders struct {
		RemindBefore  []string `yaml:"remind_before"`
		PollInterval  string   `yaml:"poll_interval"`
		Timezone      string   `yaml:"timezone"`
		SkipCompleted *bool    `yaml:"skip_completed"`
		NotifyNew     *bool    `yaml:"notify_new_assignments"`
		PruneAfter    string   `yaml:"prune_after"`
	} `yaml:"reminders"`

	StatePath string `yaml:"state_path"`
	DryRun    bool   `yaml:"dry_run"`
}

// DefaultConfigPath is used when CONFIG_PATH is unset.
const DefaultConfigPath = "config.yaml"

func configPath() string {
	if p := strings.TrimSpace(os.Getenv("CONFIG_PATH")); p != "" {
		return p
	}
	return DefaultConfigPath
}

// LoadConfig reads and validates the YAML config file. Secrets live in the file
// itself (which is gitignored), so there is no env indirection.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file %s not found (copy config.example.yaml to %s)", path, path)
		}
		return nil, err
	}
	var y yamlConfig
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject typos / unknown keys
	if err := dec.Decode(&y); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	c := &Config{
		BotToken:        y.Discord.Token,
		DMUserID:        y.Discord.DMUserID,
		GuildID:         y.Discord.GuildID,
		AssignmentsPath: strOr(y.WebAssign.AssignmentsPath, "assignments.json"),
		StatePath:       strOr(y.StatePath, "state.json"),
		DryRun:          y.DryRun,

		Source:           strings.ToLower(strOr(y.Source, "icollege")),
		D2LBase:          strOr(y.ICollege.D2LBase, "https://gastate.view.usg.edu"),
		IdPEntityID:      strOr(y.ICollege.IdPEntityID, "https://idp.gsu.edu/idp/shibboleth"),
		CourseOrgUnit:    y.ICollege.CourseOrgUnit,
		CourseName:       y.ICollege.CourseName,
		GSUUsername:      y.ICollege.GSUUsername,
		GSUPassword:      y.ICollege.GSUPassword,
		ChromeProfileDir: strOr(y.ICollege.ChromeProfileDir, "chrome-profile"),
		BrowserPath:      y.ICollege.BrowserPath,
		Headless:         boolOr(y.ICollege.Headless, true),
		SkipCompleted:    boolOr(y.Reminders.SkipCompleted, true),
		NotifyNew:        boolOr(y.Reminders.NotifyNew, true),
	}

	switch c.Source {
	case "icollege":
		c.UseICollege = true
	case "webassign":
		c.UseWebAssign = true
	case "both":
		c.UseICollege, c.UseWebAssign = true, true
	default:
		return nil, fmt.Errorf("source: want \"icollege\", \"webassign\", or \"both\", got %q", y.Source)
	}
	if c.Location, err = time.LoadLocation(strOr(y.Reminders.Timezone, "America/New_York")); err != nil {
		return nil, fmt.Errorf("reminders.timezone: %w", err)
	}
	if c.PollInterval, err = time.ParseDuration(strOr(y.Reminders.PollInterval, "60m")); err != nil {
		return nil, fmt.Errorf("reminders.poll_interval: %w", err)
	}
	if c.PruneAfter, err = time.ParseDuration(strOr(y.Reminders.PruneAfter, "168h")); err != nil {
		return nil, fmt.Errorf("reminders.prune_after: %w", err)
	}
	if c.RemindBefore, err = ParseThresholdList(y.Reminders.RemindBefore); err != nil {
		return nil, fmt.Errorf("reminders.remind_before: %w", err)
	}

	if !c.DryRun && (c.BotToken == "" || c.DMUserID == "") {
		return nil, fmt.Errorf("discord.token and discord.dm_user_id are required (or set dry_run: true)")
	}
	if c.UseICollege {
		if c.CourseOrgUnit == "" {
			return nil, fmt.Errorf("source icollege needs icollege.course_org_unit")
		}
		if c.GSUUsername == "" || c.GSUPassword == "" {
			return nil, fmt.Errorf("source icollege needs icollege.gsu_username and icollege.gsu_password")
		}
	}
	return c, nil
}

// ParseThresholdList parses reminder offsets like ["72h","24h","6h","1h"] (any order,
// Go durations plus "d" for days), sorted largest first with duplicates removed.
// An empty list falls back to the default set.
func ParseThresholdList(parts []string) ([]Threshold, error) {
	if len(parts) == 0 {
		parts = []string{"72h", "24h", "6h", "1h"}
	}
	var out []Threshold
	seen := map[time.Duration]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := parseDurationDays(part)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("threshold %q must be positive", part)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, Threshold{Label: part, D: d})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no thresholds given")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].D > out[j].D })
	return out, nil
}

// ParseThresholds parses a comma-separated threshold string (used in tests).
func ParseThresholds(s string) ([]Threshold, error) {
	return ParseThresholdList(strings.Split(s, ","))
}

func parseDurationDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func strOr(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func boolOr(v *bool, def bool) bool {
	if v != nil {
		return *v
	}
	return def
}
