// Command notifier reminds you of upcoming WebAssign assignments over Discord DMs.
//
// Sources (config.yaml "source"): icollege (iCollege/D2L via headless Chrome login),
// webassign (a page scrape written to assignments.json by Claude in Chrome), or both.
//
// Usage:
//
//	notifier        run one pass and exit (cron/timer friendly)
//	notifier bot    run as an always-on service with Discord slash commands
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
	_ "time/tzdata" // don't depend on the host's zoneinfo for America/New_York
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("notifier: ")

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := LoadConfig(configPath())
	if err != nil {
		log.Fatal(err)
	}
	st, err := LoadState(cfg.StatePath)
	if err != nil {
		log.Fatalf("loading state: %v", err)
	}
	dm := &DM{Token: cfg.BotToken, UserID: cfg.DMUserID, Client: &http.Client{Timeout: 20 * time.Second}}
	app := NewApp(cfg, st, dm)
	defer app.Close()

	switch cmd {
	case "", "run":
		if err := app.Pass(context.Background()); err != nil {
			log.Fatal(err)
		}
	case "bot":
		if err := runBot(context.Background(), app); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command %q (use \"run\" or \"bot\")", cmd)
	}
}
