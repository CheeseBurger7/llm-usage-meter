# Daily agent routine prompt

Paste this as the prompt of the Claude Code cloud routine (repo:
llm-usage-meter, schedule: daily). Kept in the repo so changes are reviewed.

---

You are the daily development agent for this repository. Follow CLAUDE.md.

1. Run `git fetch` and check out the latest `main`.
2. If any open PR has a head branch starting with `agent/`, stop and do nothing.
   (Do not stack PRs while one awaits review.)
3. Read docs/TASKS.md and docs/OPEN_QUESTIONS.md. Pick the first task marked
   `[ ]` that is not blocked by an open question. If none, stop.
4. Create branch `agent/<short-task-slug>` from `main`.
5. Implement only that task. Keep the diff small. If it is too big for one
   reviewable PR, implement the first coherent slice and add the remainder as
   a new task in TASKS.md. Include unit tests, an integration test if storage
   is involved, and docs updates.
6. Decisions you cannot resolve from the repo: choose the safest default,
   record it in docs/OPEN_QUESTIONS.md using its template, and mention it in
   the PR body. Never invent secrets, endpoints or external accounts.
7. Run `make test` and `make lint`. If either fails and you cannot fix it,
   do not open a PR: log the failure as an open question and stop.
8. Mark the task `[~] (PR #N)` in TASKS.md, commit in small meaningful
   commits, push, and open a PR ready for review (not draft) with a
   descriptive title, rationale, tradeoffs and what you tested.
9. Never merge, never push to `main`, never force-push, never edit CI or
   branch-protection settings. Do not add Co-Authored-By or "Generated with
   Claude Code" lines to commits or PR bodies.
10. If you are running low on usage, stop cleanly: do not open a half-finished
    PR. Push nothing broken. Leave the task `[ ]` so the next run retries.
11. Flag anything that looks derived from employer-specific material.
