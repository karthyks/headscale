# Fork baseline notes

Date: 2026-08-22. Fork point: `565fd254` (2026-07-29) — byte-identical to upstream
`main` HEAD on this date (0 commits behind, verified via `git fetch upstream --tags`
+ `git log 565fd254..upstream/main`).

## Verdict: does the fork contain issue #3417 behaviour?

**YES.** The mechanism is present at the fork point:

- `hscontrol/state/state.go:3306-3336` (`buildMapRequestChangeResponse`):
  - Any `hostinfoChanged` map request returns `change.NodeAdded(id)` (:3312-3314).
    Clients send hostinfo updates routinely (service lists, version strings), so
    ordinary churn escalates to full-update broadcasts.
  - The final fallback (:3335) *also* returns `NodeAdded` when a request is neither
    hostinfo-, endpoint-, nor DERP-classified.
- `hscontrol/types/change/change.go:441-442`: `NodeAdded` wraps
  `PeersChanged("node added", id)` — a broadcast to every peer, each of which then
  runs visibility filtering and peer-map rendering (the O(N·P·M) fan-out path,
  `mapper/mapper.go:446-496`).

This matches upstream issue
[#3417](https://github.com/juanfont/headscale/issues/3417) (2026-08-10, v0.29.3:
full-netmap rebuilds fanned at 8–14× connection rate, ~3× steady-state CPU vs 0.28,
subnet-route blackholes from map-delivery timeouts). No upstream fix exists yet
(upstream `main` == fork point); remediation is fork work — see plan Task 1.5.

## Related structural facts (static analysis at 565fd254)

- O(N²·M) `BuildPeerMap` (`policy/v2/policy.go:606-702`) runs on the NodeStore's
  single writer goroutine (`state/node_store.go:383-416`, unbuffered chan :369);
  every map request blocks on `<-work.result` (:218). CPU pins to ~one core.
- Persist cascade doubles rebuild cost: DB UPDATE → `SetNodes(N)` →
  `RebuildPeerMaps()` (`state/state.go:3228` → `:2901` → `node_store.go:982`).
- Broadcast patches re-filter visibility per recipient
  (`mapper/mapper.go:479-496` → `visiblePeerIDs` :446-473).
- Tick drain is single-goroutine (`batcher.go:441-467`), tick budget 800 ms
  (`tuning.batch_change_delay`, `config.go:489`); sync initial maps share the bulk
  work queue (cap `workers*200`, :46) with a 5 s send timeout (:338).

Full analysis: `.hermes/plans/2026-08-22_084900-headscale-fork-scaling.md`
(workspace-level); upstream research with citations:
`~/workspace/headscale-scalability-research.md`.
