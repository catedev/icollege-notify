package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigICollege(t *testing.T) {
	cfg, err := LoadConfig(writeCfg(t, `
discord:
  token: tok
  dm_user_id: "42"
source: icollege
icollege:
  gsu_username: user
  gsu_password: pass
  course_org_unit: "1234567"
  headless: false
reminders:
  remind_before: ["2d", "6h"]
  poll_interval: 30m
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UseICollege || cfg.GSUUsername != "user" || cfg.CourseOrgUnit != "1234567" {
		t.Errorf("icollege fields wrong: %+v", cfg)
	}
	if cfg.Headless {
		t.Error("headless:false should disable headless")
	}
	if cfg.SkipCompleted != true || cfg.NotifyNew != true {
		t.Error("omitted bools should default true")
	}
	// Sorted largest first: 2d then 6h.
	if len(cfg.RemindBefore) != 2 || cfg.RemindBefore[0].Label != "2d" || cfg.RemindBefore[1].Label != "6h" {
		t.Errorf("thresholds wrong: %+v", cfg.RemindBefore)
	}
	if cfg.D2LBase == "" || cfg.PollInterval.Minutes() != 30 {
		t.Errorf("defaults/overrides wrong: base=%q poll=%v", cfg.D2LBase, cfg.PollInterval)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "source: icollege\nnope: 1\n",
		"bad source":        "source: blackboard\ndry_run: true\n",
		"missing discord":   "source: webassign\n",
		"icollege no creds": "source: icollege\ndiscord:\n  token: t\n  dm_user_id: u\nicollege:\n  course_org_unit: \"1\"\n",
	}
	for name, body := range cases {
		if _, err := LoadConfig(writeCfg(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadConfigDryRunSkipsDiscordCheck(t *testing.T) {
	if _, err := LoadConfig(writeCfg(t, "source: webassign\ndry_run: true\n")); err != nil {
		t.Errorf("dry_run should not require discord creds: %v", err)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("expected error for missing file")
	}
}
