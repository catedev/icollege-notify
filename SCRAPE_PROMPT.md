Using the Claude in Chrome tools, read my WebAssign "My Assignments" page and write the
assignment list to ./assignments.json. This is strictly read-only.

Steps:
1. Call tabs_context_mcp. If a webassign.net tab exists, use it. Otherwise open
   https://www.webassign.net/v4cgi/student.pl in a new tab (and close it when done).
2. If the page isn't already My Assignments, click the nav link "My Assignments".
   Do not hard-code the `?q=` token. Stay on the default "Show Current Assignments" tab.
3. Confirm the page title ends in "- My Assignments" (the click can take a moment; the
   Home page also lists assignments but without scores). Then read it with read_page
   (filter "all"). get_page_text gives names, categories, due dates and scores but
   DROPS the "Timed Assignment" marker in the Restrictions column, so use read_page
   (or find) for that. Do NOT rely on javascript_tool. Take each assignment ID from
   the link href `/web/Student/Assignment-Responses/last?dep=<ID>`.
   Due times vary per assignment (not always 11:59 PM): copy each one exactly.
4. If a Cengage sign-in screen appears instead, stop and write
   {"scraped_at": "<now, RFC3339>", "error": "login_required", "assignments": []}

Never: type credentials, click the OneTrust cookie/consent dialog, open/start/submit any
assignment, or click any timed quiz.

Output, written first to ./assignments.json.tmp and then moved with
`mv assignments.json.tmp assignments.json`:

{
  "scraped_at": "<now, RFC3339 with offset>",
  "course": "<page title before ' - My Assignments'>",
  "error": null,
  "assignments": [
    {
      "id": "10000774",
      "name": "7.3 Homework: Probability and Odds",
      "category": "Homework",          // text in parentheses, no parens
      "timed": false,                       // true if row says "Timed Assignment"
      "due_raw": "Monday, November 2, 2026 at 11:59 PM EST",
      "due": "2026-11-02T23:59:00-05:00",  // America/New_York; EDT=-04:00, EST=-05:00
      "score": null,                        // number, or null when shown as "-"
      "max_score": 100,
      "url": "https://www.webassign.net/web/Student/Assignment-Responses/last?dep=10000774"
    }
  ]
}

(The comments above are explanation only; the file must be plain JSON.)
Reply with just the number of assignments written, or "login_required".
