# feature-flags

[![CI](https://github.com/MahyarNemati/feature-flags/actions/workflows/ci.yml/badge.svg)](https://github.com/MahyarNemati/feature-flags/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/MahyarNemati/feature-flags.svg)](https://pkg.go.dev/github.com/MahyarNemati/feature-flags/sdk/go)

A feature-flag service, a Go SDK and a dashboard: percentage rollouts with
**deterministic bucketing**, attribute targeting, a kill switch, full audit
history, and an SDK that evaluates locally so a flag check can never fail.

```bash
export FLAGD_ADMIN_TOKEN=$(openssl rand -hex 16)
go run ./cmd/flagd -seed flags.example.json
# dashboard on http://localhost:8080
```

```go
flags, _ := flagsdk.New(flagsdk.Options{BaseURL: "http://localhost:8080", Token: token})
flags.Connect(ctx)

if flags.Bool("new-checkout", flagsdk.Context{
    Key:        user.ID,
    Attributes: map[string]any{"email": user.Email, "country": user.Country},
}, false) {
    return newCheckout(w, r)
}
return legacyCheckout(w, r)
```

## The two properties everything else follows from

### 1. Bucketing must be deterministic

A rollout that reshuffles on each evaluation is not a rollout, it is a coin
flip per request — and a user who watches a feature appear and disappear files
a bug you cannot reproduce.

So a subject's position is `SHA-256(flagKey, ruleID, subject) mod 10000`. No
randomness, no state, no clock. Three details in that formula are deliberate:

- **The rule ID is a salt.** Without it, the same unlucky 10% of users would be
  the guinea pigs for every experiment forever.
- **Inputs are length-prefixed**, so `("ab","c")` cannot collide with
  `("a","bc")`. There is a test for that.
- **SHA-256, not a fast hash.** This value has to match across SDKs in
  different languages for years. A non-cryptographic hash is faster and a
  worse choice: implementations differ in seed and endianness, and "the Go SDK
  and the Python SDK disagree about who is in the rollout" is a miserable bug.

Weights are **basis points** (1/100th of a percent) and must sum to exactly
10000. Integers because a rollout must be reproducible across languages, and
float arithmetic is not; basis points because "5.5% of users" is a real
request that an integer percentage cannot express. Anything but an exact total
is rejected, since the alternative is silently dropping or duplicating traffic
with no visible error.

A 200,000-sample test asserts a 20/80 split lands within 0.5 percentage
points, and CI runs the bucketing tests separately with extra repetitions —
because a change to the hash silently reshuffles every live rollout.

### 2. The SDK must never be able to break the application

A flag system becomes a dependency of every code path. If it can throw, hang
or crash, it has made the application *less* reliable than it was before flags
existed. So:

- **Evaluation is local**, against an in-memory snapshot. No network call on
  the hot path, so a flag check adds no latency and cannot fail.
- **The snapshot refreshes in the background** over SSE, with polling as a
  fallback. A dead server means flags go *stale*, not down — there is a test
  that closes the server and keeps evaluating.
- **`Connect` does not fail when the server is down.** A flag service that is
  unreachable at deploy time must not stop the application from starting. It
  logs and retries; `WaitReady` is there for callers who genuinely need flags
  before serving traffic.
- **Every accessor takes a fallback and returns it on any problem** — missing
  flag, wrong type, malformed definition. There is no error return on the hot
  path, because there is no action a caller could take other than using the
  fallback.
- **Nothing panics.**

## Flags

Four kinds — `bool`, `string`, `int`, `json` — so the API returns a typed value
rather than making every call site parse a string. A string flag means a third
option does not need a second flag; a JSON flag keeps related configuration
together.

```json
{
  "key": "new-checkout",
  "kind": "bool",
  "enabled": true,
  "variations": [
    { "key": "on",  "value": true },
    { "key": "off", "value": false }
  ],
  "default_variation": "off",
  "off_variation": "off",
  "rules": [
    {
      "id": "internal-staff",
      "conditions": [
        { "attribute": "email", "operator": "ends_with", "values": ["@acme.com"] }
      ],
      "variation": "on"
    },
    {
      "id": "ramp-uk",
      "conditions": [
        { "attribute": "country", "operator": "in", "values": ["GB", "IE"] }
      ],
      "rollout": {
        "weights":   { "on": 1000, "off": 9000 },
        "bucket_by": "account_id"
      }
    }
  ]
}
```

See [`flags.example.json`](flags.example.json), which CI loads and exercises.

### `enabled` is the kill switch

Setting `enabled: false` serves `off_variation` **regardless of every rule**.
That is the whole point: during an incident you turn the feature off, you do
not unpick its targeting first. The audit log records it as `disable` rather
than a generic `update`, so a kill-switch flip is obvious in the history.

### `bucket_by` keeps a group together

By default a rollout buckets on the targeting key, usually a user id. Setting
`bucket_by: "account_id"` puts a whole account on the same side — which is what
you want when the feature changes shared state and half a team seeing the new
version would be worse than none of them.

When the attribute is missing it falls back to the targeting key, **not** to
the empty string: the latter would put every subject lacking the attribute in
one bucket and silently skew the rollout.

### Targeting operators

`eq`, `neq`, `in`, `not_in`, `contains`, `starts_with`, `ends_with`, `gt`,
`lt`, `semver_gte`, `semver_lt`, `regex`, `exists`, each with an optional
`negate`. Conditions within a rule are ANDed; rules are evaluated in order and
the first match wins.

Two behaviours worth knowing:

- **Semver comparison is numeric.** Lexically `"1.10.0" < "1.9.0"`, so a string
  comparison would exclude exactly the newer clients a version rule is meant to
  include. Pre-release and build metadata are ignored, so `1.10.0-beta.1`
  compares as `1.10.0`.
- **A missing attribute matches nothing**, including `neq`. Treating it as an
  empty string would make `plan != "pro"` true for a context with no plan at
  all — a surprising way to leak a feature to everyone anonymous.

### Validation is strict, at definition time

A flag is rejected if a fallback variation does not exist, a rule serves an
undeclared variation, rule IDs collide (the ID is the rollout salt), a regex
does not compile, or rollout weights do not total exactly 100%. Catching these
when the flag is *saved* is far better than discovering them during evaluation,
on a request.

## Evaluation reasons

Every evaluation says **why**, which is what makes the system debuggable:

| Reason | Meaning |
|---|---|
| `off` | the flag is disabled |
| `rule_match` | a targeting rule matched; `rule_id` names it |
| `rollout` | a rule matched and a percentage rollout chose; `bucket` is the subject's position |
| `default` | the flag is on and no rule matched |
| `flag_not_found` | no such flag; your fallback was used |
| `error` | the flag is malformed; `error` explains, your fallback was used |

"Why am I not seeing the feature?" has an answer, including the exact bucket
when a rollout is involved.

## API

| Method | Path | Token |
|---|---|---|
| `POST` | `/api/v1/evaluate` | any |
| `POST` | `/api/v1/evaluate/all` | any |
| `GET` | `/api/v1/snapshot` | any — ETag aware |
| `GET` | `/api/v1/stream` | any — SSE |
| `GET` | `/api/v1/flags`, `/api/v1/flags/{key}` | any |
| `POST` `PUT` `DELETE` | `/api/v1/flags...` | **admin** |
| `GET` | `/api/v1/history`, `/api/v1/flags/{key}/history` | any |
| `GET` | `/healthz` `/readyz` | none |

Design decisions a reviewer might ask about:

- **A missing flag is a 200, not a 404.** The caller asked "what should I
  serve?" and the answer is "your fallback". Making it an error forces every
  call site to handle a condition that has an obvious correct behaviour.
- **`If-Match` carries the version on update**, so two people editing the same
  flag in a dashboard get a `412` instead of silently overwriting each other.
  The loser is told; without it, they would never know.
- **Unknown JSON fields are rejected.** A typo'd `enabledd` that is silently
  ignored is a flag that does not behave the way its author believes it does.
- **No tokens configured means the API is closed**, not open. A flag service
  anyone can write to lets anyone turn features on in production, so `flagd`
  refuses to start without a token at all.
- **SSE rather than WebSockets** for streaming: the traffic is
  one-directional, SSE reconnects on its own, survives proxies that only speak
  HTTP, and needs no extra protocol handling in an SDK. The stream sends the
  **current state immediately**, so a client is correct from its first byte
  rather than only after something happens to be edited.

## Audit history

Every change records the actor (from the token's name — which is why tokens are
named rather than one shared secret), the action, an optional comment, and the
**full before and after snapshot**. Snapshots rather than diffs, so history
survives schema changes and can answer "what exactly was being served at 14:00
last Tuesday".

## Dashboard

Server-rendered, no build step, no JavaScript framework — a dashboard that
needs a toolchain is a dashboard that stops working in a year. It lists flags,
shows each one's variations, rules and history, and offers exactly one
mutation: the on/off toggle, because that is the one that matters in an
incident.

It deliberately has **no session of its own**; put real authentication in front
of it. Without an admin token it is read-only rather than open.

## Layout

```
internal/flag/    the model and its validation
internal/eval/    evaluation, targeting and deterministic bucketing
internal/store/   persistence, audit history, change notification
internal/api/     REST, evaluation, snapshot and SSE
internal/web/     server-rendered dashboard
cmd/flagd/        the daemon
sdk/go/           the Go client
```

## Development

```bash
make test        # unit + integration
make test-race   # with the race detector (needs working cgo)
make run         # flagd with the example flags
make demo        # start it and evaluate one flag
```

CI runs the race detector (the SDK swaps an atomic snapshot pointer while
readers evaluate, so it is doing real work), re-runs the bucketing and
distribution tests with extra repetitions, and runs the daemon end to end:
loading the example flags, checking that targeting, rollouts, semver and
fallbacks behave exactly as this README claims, that a read-only token cannot
write, that the stream pushes a change, that the audit log records it, and that
the dashboard renders.

## Limitations

- **Storage is in-memory**, so a restart loses flags. The `store.Store`
  interface is the seam for Postgres; `-seed` plus a file in git is a
  legitimate GitOps-style deployment in the meantime.
- **No percentage rollouts without a rule.** A rollout lives inside a targeting
  rule, so a flag that should ramp for everyone needs a rule with an `exists`
  condition on `key`. Visible in `flags.example.json`.
- **No experiment analysis.** It assigns variations deterministically and
  reports the bucket; it does not collect exposure events or compute
  significance. Pair it with your analytics pipeline.
- **One environment.** Separate dev and prod means running two instances. Real
  environment support needs an environment dimension through the model, the
  API and the tokens.
- **The SDK is Go only.** The bucketing algorithm is specified precisely enough
  above to port, and that is deliberate — but there is no second
  implementation proving the specification is complete.
- **No scheduled changes** or approval workflow.

## License

MIT — see [LICENSE](LICENSE).
