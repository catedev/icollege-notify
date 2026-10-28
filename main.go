// Command icollege-notify sends Discord DM reminders for upcoming iCollege/WebAssign assignments.
//
// Usage:
//
//	icollege-notify        one pass and exit
//	icollege-notify bot    always-on, with slash commands
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
	log.SetPrefix("icollege-notify: ")

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
