# webassign-notify

Discord reminders for upcoming WebAssign / iCollege assignments. A Go program tracks
state and DMs you; reminder, completion, and new-assignment logic live in one place and
are unit-tested without a browser.

## Data sources

**A. iCollege / D2L direct (`source: icollege`)** — server-friendly, no live browser
session to babysit. A headless Chromium-based browser (Helium is auto-detected; override with `icollege.browser_path`) signs in to GSU once and reads due dates and
synced scores straight from D2L's API. Covers the whole semester. (It can't see a running
quiz timer or a personal extension — those live only in WebAssign.)

**B. WebAssign page scrape (`source: webassign`)** — `claude -p --chrome` reads the My
Assignments page in your signed-in Chrome and writes `assignments.json` (see
`SCRAPE_PROMPT.md`), then `./notifier` processes it. Catches per-student quiz timers.

**C. Both (`source: both`)** — iCollege is the base list; WebAssign (`assignments.json`)
is merged in by assignment name. An assignment is treated as **done if it's submitted on
either platform**, so you won't get reminders for work already turned in on one side while
the other's grade sync lags. WebAssign's per-student due times (e.g. a running quiz timer)
override iCollege's course-wide date. If `assignments.json` is missing or stale, iCollege
is used alone — so keep refreshing it with the Chrome scrape (`run.sh`) when you want the
WebAssign signal fresh.

## Run modes

- `notifier` — one pass, then exit. Fine with cron if you don't need slash commands.
- `notifier bot` — **always-on service** with Discord slash commands. Polls on
  `reminders.poll_interval` internally. This is the recommended way to run iCollege mode.

## Setup

1. Create an application at <https://discord.com/developers/applications>, add a **Bot**,
   copy its token. Invite it to a server you're in (OAuth2 → URL Generator → scope
   `applications.commands` + `bot`). A bot can only DM users it shares a server with.
2. Configure:
   ```
   cp config.example.yaml config.yaml && chmod 600 config.yaml
   ```
   Fill in `discord.token`, `discord.dm_user_id`, and (for instant slash-command
   registration) `discord.guild_id`. For iCollege mode set `source: icollege` and
   `icollege.gsu_username` / `gsu_password` / `course_org_unit` (the D2L org unit id from
   myenrollments). **Secrets live in `config.yaml`, which is gitignored.** Point elsewhere with
   `CONFIG_PATH=/etc/webassign-notify.yaml`.
3. `go build -o notifier .`
4. Install the service (runs in your user session so desktop notifications work as a
   last-resort channel):

   ```
   cp systemd/webassign-notify.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now webassign-notify.service
   journalctl --user -u webassign-notify -f
   ```

Dry run without Discord — in `config.yaml` set `source: webassign`,
`webassign.assignments_path: testdata/assignments.json`, `dry_run: true`, then `./notifier`.

## Slash commands

- **`/login`** — sign in to iCollege again. Use this if a login **timed out** (you missed
  the 2FA prompt). It clears the don't-retry latch, re-runs sign-in, and DMs you for the
  Duo code.
- **`/refresh`** — fetch now and send any due reminders.
- **`/assignments`** — list current upcoming, incomplete assignments.

Only your `discord.dm_user_id` can use them; anyone else gets a refusal.

## Staying signed in (iCollege mode)

- **Persistent profile.** `icollege.chrome_profile_dir` keeps cookies between runs, including Duo's
  **"remember this device"** cookie, which the login flow ticks automatically. So a full
  sign-in happens only when the session truly expires, not every poll.
- **Keep-alive.** Each poll pings an authenticated endpoint, refreshing D2L's idle timer so
  the session survives between fetches.
- **2FA.** On a real re-login the bot DMs you for a Duo passcode (or waits for a push). No
  reply in time → it backs off and you resume with **`/login`**.
- **Account safety.** If GSU rejects the password, the bot stops trying until the
  credentials in `config.yaml` change (or you run `/login`), so it can't lock your account by
  retrying. Only a hash of the credentials is stored, never the password.
- **Discord token.** Doesn't expire; only breaks if you reset it. On a 401 the bot can't DM
  you, so it raises one critical desktop notification instead.

Note: automating GSU single sign-on with stored credentials may be against the university's
acceptable-use policy. That's your call to make.

## Behaviour

- Reminders at `reminders.remind_before` (default `72h,24h,6h,1h`). Only the smallest crossed
  threshold is sent; larger ones are marked sent too, so a late first run sends one message.
- `score == max_score` counts as done and is skipped.
- New assignment IDs are announced, except on the first run (which just records state).
- One-shot warnings for login/fetch failures and for data older than 3× `reminders.poll_interval`.
- State is marked only after a DM is delivered, so failed sends retry next pass.
- Entries more than `reminders.prune_after` (7 days) past due that have left the list are pruned.
