# Changelog

Notable changes, newest first. Breaking changes are marked **Breaking**.

## Unreleased

- The proxy no longer records a call that ended in a refusal before any output unless the
  refusal category is one the provider bills (Q21): `bio`, `frontier_llm` and
  `reasoning_extraction` by default, replaced by `PROXY_BILLED_REFUSAL_CATEGORIES`. Such calls
  were recorded before, which charged tenants for refusals the provider did not bill. Skipped
  refusals are counted as `refusals_unbilled` or `refusals_unknown_category`. See
  [docs/proxy.md](docs/proxy.md#refusals).

- Events take an optional `partial` boolean, default `false`, on `POST /v1/events`,
  `POST /v1/events/batch` and in the Go client (Q19). It marks a call whose counts are known to
  be incomplete, such as a stream the caller hung up on. `hourly_usage` gains `partial_count`.
  Migration 00008 adds both columns; existing rows read `false` and `0`. Not breaking.

- **Breaking:** ingest refuses an event whose `provider`, after `A`-`Z` is
  lowercased, does not match `^[a-z0-9._-]{1,256}$` (Q20). Whitespace,
  non-ASCII letters and other punctuation now get `400` with a field error
  on `POST /v1/events`, and `invalid` on `POST /v1/events/batch`. Uppercase
  ASCII is still accepted and folded (Q13). Migration 00007 adds the same
  rule as a `CHECK` on `events.provider` and `hourly_usage.provider`. The
  proxy refuses to start when `CHAT_COMPLETIONS_PROVIDER` breaks the rule.
  See [docs/ingestion.md](docs/ingestion.md).
