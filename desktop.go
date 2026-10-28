package main

import (
	"log"
	"os/exec"
)

// desktopNotify is the fallback channel for problems that stop Discord delivery.
// It's best-effort: without notify-send or a desktop session it only logs.
func desktopNotify(title, body string) {
	log.Printf("%s: %s", title, body)
	if path, err := exec.LookPath("notify-send"); err == nil {
		if err := exec.Command(path, "--urgency=critical", "--app-name=webassign-notify", title, body).Run(); err != nil {
			log.Printf("notify-send: %v", err)
		}
	}
}
