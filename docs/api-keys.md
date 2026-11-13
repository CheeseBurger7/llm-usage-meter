# API keys (`internal/auth`)

An API key ties a request to one org and one scope. This page covers how keys
are generated, stored, verified, managed from the command line and checked on
each HTTP request. The tables are described in
[orgs-and-keys.md](orgs-and-keys.md).

## Key format

```
lum_<scope>_<64 hex characters>
```

- `scope` is `ingest` (write events) or `read` (query usage and cost).
- The last part is 32 bytes from `crypto/rand`, hex-encoded.
- The **prefix** is the first 8 characters of that last part. It is stored in
  clear, shown to humans and used to revoke a key. It is not a secret.

Only the SHA-256 hash of the whole key and the prefix are stored. The
plaintext is printed once, when the key is created, and cannot be recovered.

## Managing orgs and keys

All three commands need `DATABASE_URL` and apply pending migrations first.

```sh
# Create an org. Prints the org id.
go run ./cmd/server orgs create --name "Acme"

# Create a key. Prints the plaintext key, once.
go run ./cmd/server keys create --org <org id> --scope ingest

# Revoke by the 8-character prefix shown at creation.
go run ./cmd/server keys revoke --prefix 5d4a5dcc
```

The value a script needs (the org id or the key) is the only thing written to
stdout, so `KEY=$(server keys create ...)` works. Notes for humans go to
stderr and never include the key. These commands do not use the JSON logger,
so a new key cannot end up in the service logs.

`keys revoke` exits non-zero when no active key has that prefix, so a typo
does not look like a successful revocation. Prefixes are not unique: if two
active keys share one, both are revoked and the count is printed.

## Verification

`auth.Service.Verify(ctx, key)` returns the org id and scope, or:

| Error | Meaning |
|---|---|
| `ErrInvalidKey` | Malformed, or no such key. The two are not distinguished |
| `ErrRevokedKey` | The key existed and has been revoked |

A malformed key is rejected before any database query. A well-formed key
costs one indexed lookup on `key_hash`. The scope returned is the one stored
with the key, not the one written in the key text, and because the hash
covers the whole key, changing the scope label gives an unknown key.

## HTTP authentication

Every route except `GET /healthz` needs a key:

```
Authorization: Bearer lum_ingest_<64 hex characters>
```

| Route | Scope |
|---|---|
| `GET /healthz` | none, always open |
| `POST /v1/events` | `ingest` |
| `POST /v1/events/batch` | `ingest` |

| Status | When | Body |
|---|---|---|
| `401` | No header, a scheme other than `Bearer`, or an empty or malformed value | `{"error":"missing or malformed Authorization header, want: Bearer <key>"}` |
| `401` | The key is unknown or revoked | `{"error":"invalid API key"}` |
| `403` | The key is valid but has another scope | `{"error":"this route needs a key with the ingest scope"}` |
| `500` | The key could not be checked, for example the database is down | `{"error":"internal error"}` |
| `429` | The key is valid but has sent too many ingest requests; see [rate limit](ingestion.md#rate-limit) | `{"error":"rate limit exceeded, ..."}` with `Retry-After` |

Every `401` carries `WWW-Authenticate: Bearer`. The check runs before the
request body is read, so a rejected request never reaches the store.

`auth.Require(verifier, scope, log)` is the middleware, applied per route in
`api.NewRouter`. A handler behind it reads the org with
`auth.IdentityFromContext(r.Context())`. The org always comes from the key,
never from the request body. Events are stored under that org; see
[ingestion.md](ingestion.md#org-scoping).

The key is never logged. The middleware logs one thing, the error when a key
cannot be checked, and that error does not contain the key or the header.

## Design decisions

- **Unknown and revoked keys get the same `401`.** A different answer for a
  revoked key would confirm to whoever holds a leaked key that it was once
  real. The service still tells them apart internally.
- **A wrong scope is `403`, not `401`.** The key is real, so the caller is
  told which scope the route needs. This only reveals something to a caller
  who already holds a valid key for the org.
- **A failed check is `500` and the request is refused.** Letting requests
  through when the database is down would accept unauthenticated writes. The
  client retries, which is safe because ingestion is idempotent.
- **One database lookup per request, no cache.** It is one indexed read and a
  revocation takes effect on the next request. A short in-memory cache would
  save the read but keep a revoked key working until the entry expires. It
  can be added once the lookup shows up in latency.
- **Middleware per route, not around the whole router.** Each route states
  its scope where it is registered, and `/healthz` needs no exception list.

- **SHA-256, not a password hash.** The secret has 256 bits of entropy, so it
  cannot be guessed offline even with a fast hash. bcrypt or argon2 would add
  tens of milliseconds to every request for no gain. No salt is needed for
  the same reason, and an unsalted hash is what allows lookup by index.
- **Hex encoding.** Hex gives a 75-character key. base62 would be about 20
  characters shorter but needs extra code, and base64url contains `_` and
  `-`, which makes the key harder to split and to select by double-click.
  See Q12 in [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
- **The prefix is part of the secret.** Showing 8 hex characters reveals 32 of
  the 256 random bits; 224 remain. The alternative, a separate public key id,
  makes the key longer for no practical gain.
- **`OrgID` is a plain `[16]byte` type.** Callers of `internal/auth` do not
  need to import a database driver type to carry an org id around.
- **Revoked and unknown keys are separate errors** so the service can log and
  count them separately. The middleware answers 401 to both.

## Tests

- `internal/auth/key_test.go`: key shape and uniqueness, the parser rejecting
  malformed keys, the hash, scope and org id parsing.
- `internal/auth/service_integration_test.go` (Postgres via testcontainers,
  skipped without Docker): verify returns the right org and scope, only the
  hash and prefix are stored, unknown and relabelled keys are rejected, a key
  for a missing org fails, and revocation is idempotent and does not touch
  other keys.
- `cmd/server/admin_test.go`: the three commands, their flag errors, and that
  the key is written to stdout only.
- `internal/auth/middleware_test.go`: every status above, the identity in the
  request context, unknown and revoked keys answering identically, and the
  key appearing in neither the log nor the response.
- `internal/api/router_test.go`: `/healthz` is open, `POST /v1/events` needs
  an `ingest` key, and a rejected request does not reach the store.
- `internal/api/events_integration_test.go` (Postgres): real keys over HTTP,
  including a revoked key (`401`) and a `read` key (`403`).
