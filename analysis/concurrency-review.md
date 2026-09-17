# Concurrency review — after v0.1.6

Reviewed 2026-09-09. Baseline: `v0.1.6` (`bdc54d2`).

## Findings and changes

1. **Breaker outcomes could overwrite newer decisions.** A slow successful
   request and a faster 429 could share a key; the success then unconditionally
   cleared the new breaker. This is a logical race even with all field accesses
   protected by a mutex. Requests now retain an admission generation, checked
   before changing breaker state. Opening a breaker or admitting a half-open
   probe advances that generation. Old successes, failures and cancellations
   cannot modify a newer generation. Deadline/cancellation releases a probe
   without declaring the account recovered. Cancellation stops further spills.
2. **Model/readiness cache locks covered network I/O.** An upstream stall
   serialized callers on a mutex, where request cancellation could not wake
   them. Refreshes now run outside the lock and share a completion channel;
   each waiter selects between completion and its own cancellation. Shared
   refreshes use an independent 10-second context, so the initiating client's
   disconnect cannot invalidate other callers' work. They can outlive a caller
   by at most that timeout when the upstream honors context, as the HTTP client
   does. Readiness caches both outcomes for 30 seconds and waits for a fresh
   probe after expiry; it never serves expired success during that probe.
3. **Failed model refreshes retried for every queued caller.** Model queries
   now use a 30-second failure backoff, keep the last successful value, and
   serve stale data during refresh. Cold callers share the refresh. Successful
   entries retain their five-minute TTL. Separate passthrough credentials no
   longer share a model list or block each other's query. The cache keeps at
   most 128 entries, indexes by SHA-256 of the credential, evicts idle entries,
   and returns static fallback if every slot is busy. Raw keys are not retained
   in the cache index. The static catalog still does not guarantee plan access.
4. **Repeated text concatenation caused quadratic copying.** SSE handlers
   accumulated full text/reasoning that they never read. They now only relay
   deltas. Non-streaming aggregation uses `strings.Builder` for both fields.
5. **An unused terminal-report hook guessed account ownership.** It had no
   runtime caller and selected the first healthy key, which need not own the
   stream. Removed it rather than leaving an unsafe future integration point.
   Existing handling of in-band errors is unchanged; this work does not add
   stream-to-account feedback.

The ordinary key selection lock already covered only short state operations,
not the generation network call. It remains a mutex; fill-first selection and
account/session identity rules remain in effect. No speculative lock-free
replacement or new dependency was introduced.

## Verification

- `go vet ./...` and `go test -race -count=1 ./...` from `proxy/`.
- Controlled overlapping requests reproduce the late-success/503/plan-error/
  spend-cap breaker race on v0.1.6; all four cases pass after the change.
- Old-generation results cannot release an active half-open probe or break a
  recovered account; 64 concurrent admissions yield exactly one probe.
- Caller cancellation stops waiting without canceling the shared fetch;
  64 callers reuse one fetch, timeout failures are cached, and expired failure
  entries can recover. Stale model data is available during a blocked refresh.
- A blocked account's model fetch does not block other accounts or expose its
  catalog. Capacity tests cover eviction and entries still in use. Readiness
  switches from expired success to 503 on probe failure and caches the failure.

These are local tests with controlled upstream substitutes, not a live upstream
load test. No paid generation requests or `.env` changes were needed.

## Relay benchmark

Identical `BenchmarkChatRelay` test code ran against a temporary export of
v0.1.6 and the changed tree. Input contains 1,024 text deltas and 1,024 reasoning
deltas of 32 bytes each: 64 KiB of visible output total. Both versions parse the
same NDJSON and render the same API response. The downstream writer discards
bytes, so response-recorder buffering is excluded.

```sh
cd proxy
go test ./internal/server -run '^$' -bench '^BenchmarkChatRelay$' -benchtime=20x -count=3
```

Linux/amd64, Intel Xeon Max 9470C, Go 1.26.3. Approximate median of three runs:

| Mode | v0.1.6 allocated bytes/request | Changed bytes/request | Reduction |
| --- | ---: | ---: | ---: |
| Non-streaming | 37,066,665 | 1,954,128 | 94.7% |
| SSE | 39,887,569 | 4,123,884 | 89.7% |

These figures measure cumulative allocations per request, not peak resident
memory. They do not imply equivalent end-to-end latency or upstream throughput
gains; network, model generation and downstream backpressure are excluded.
