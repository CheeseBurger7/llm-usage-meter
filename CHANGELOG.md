# Changelog

Notable changes, newest first. Breaking changes are marked **Breaking**.

## Unreleased

- **Breaking:** ingest refuses an event whose `provider`, after `A`-`Z` is
  lowercased, does not match `^[a-z0-9._-]{1,256}$` (Q20). Whitespace,
  non-ASCII letters and other punctuation now get `400` with a field error
  on `POST /v1/events`, and `invalid` on `POST /v1/events/batch`. Uppercase
  ASCII is still accepted and folded (Q13). Migration 00007 adds the same
  rule as a `CHECK` on `events.provider` and `hourly_usage.provider`. The
  proxy refuses to start when `CHAT_COMPLETIONS_PROVIDER` breaks the rule.
  See [docs/ingestion.md](docs/ingestion.md).
