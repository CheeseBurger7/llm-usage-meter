# Changelog

Notable changes, newest first. Breaking changes are marked **Breaking**.

## Unreleased

- The proxy records a stream that ends before `message_stop` because the caller hung up or the
  connection dropped, instead of skipping it (Q19). The event carries `partial: true`, the exact
  input and cache counts from `message_start` and the latest output count seen, so it can only
  undercount. Such calls are counted as `calls_partial` in the shutdown log line. A stream that
  carried an `error` event stays skipped: the provider's docs do not say its input is billed.
  See [docs/proxy.md](docs/proxy.md#unfinished-streams).

- The proxy warns when a call that fell back has top-level usage counts that differ from its
  `fallback_message` entry, and counts those calls as `fallback_usage_mismatches` in the
  shutdown log line (Q27). Such a call may bill the serving model's tool-loop iterations twice.
  Events are unchanged. See [docs/proxy.md](docs/proxy.md#fallback-attempts).

- No behaviour change: a declined fallback attempt without output is still not recorded. The
  provider's docs do not say which `usage.iterations` entry a `fallback` block belongs to, so
  its category cannot be read safely (Q26). [docs/proxy.md](docs/proxy.md#fallback-attempts)
  now cites the page and the SDK type checked.

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
