# icollege-notify

Discord DM reminders for upcoming iCollege (D2L) and WebAssign assignments.

It signs in to iCollege with headless Chrome, pulls due dates and scores, and DMs you
before things are due (72h, 24h, 6h, 1h by default). Finished work is skipped, and new
assignments get announced.

## Sources

Set `source` in `config.yaml`:

- `icollege`: reads straight from D2L's API. Doesn't need a browser running in the
  background.
- `webassign`: reads `assignments.json`, written by a Claude in Chrome scrape of the
  My Assignments page (see `SCRAPE_PROMPT.md` and `run.sh`). Picks up per-student
  quiz timers and extensions that iCollege can't see.
- `both`: iCollege list, with WebAssign merged in by name. Done on either side counts as
  done, and WebAssign's due times win.

## Setup

1. Make a Discord bot at <https://discord.com/developers/applications>, then invite it to
   a server you're in, with the `bot` and `applications.commands` scopes. It can only DM
   people it shares a server with.
2. `cp config.example.yaml config.yaml && chmod 600 config.yaml` and fill it in. It holds
   your secrets and is gitignored.
3. `go build .`
4. Run it as a user service:
   ```
   cp systemd/icollege-notify.service ~/.config/systemd/user/
   systemctl --user enable --now icollege-notify.service
   ```

`./icollege-notify` does one pass and exits. `./icollege-notify bot` stays up, polls,
and handles slash commands.

To try it without Discord, set `source: webassign`,
`webassign.assignments_path: testdata/assignments.json` and `dry_run: true`, then run
`./icollege-notify`.

## Commands

- `/login`: sign in again, e.g. after you missed a Duo prompt
- `/refresh`: fetch now and send anything that's due
- `/assignments`: list what's upcoming

Only the configured `discord.dm_user_id` can use them.

## Notes

- The Chrome profile keeps the session and Duo's "remember this device" cookie, so a full
  login with 2FA is rare. When it does happen, the bot DMs you the Duo code.
- If GSU rejects your password, the bot stops retrying until the credentials change, so it
  won't lock your account.
- If Discord rejects the bot token, you get a desktop notification instead of a DM.
- Automating GSU sign-in with stored credentials may break the university's acceptable-use
  policy. Your call.
