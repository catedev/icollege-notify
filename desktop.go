package main

import (
	"log"
	"os/exec"
)

// best-effort fallback when Discord delivery is broken; only logs without notify-send
func desktopNotify(title, body string) {
	log.Printf("%s: %s", title, body)
	if path, err := exec.LookPath("notify-send"); err == nil {
		if err := exec.Command(path, "--urgency=critical", "--app-name=icollege-notify", title, body).Run(); err != nil {
			log.Printf("notify-send: %v", err)
		}
	}
}
