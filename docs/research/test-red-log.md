# RED log — target-behavior tests

## broadcast_fanout

Date: 2026-08-22
File: `hscontrol/mapper/broadcast_fanout_test.go`
Scope: batcher broadcast fan-out mechanics (who receives what kind of change),
upstream issue juanfont/headscale#3417 (reconnect of an EXISTING node fans
'node added'-reason full-netmap rebuilds to all peers).

### TestNewNodeAdditionReachesAllPeers — CHARACTERIZATION, PASS

Command:

```
cd /Users/karthyks/workspace/headscale && CGO_ENABLED=1 go test ./hscontrol/mapper/ -run TestNewNodeAdditionReachesAllPeers -count=1 -v
```

Result: PASS (0.29s). `change.NodeAdded(newNode)` delivered via
`Batcher.AddWork` + deterministic `processBatchedChanges` drain reaches both
peers as a changed-peers payload containing the new node; the added node
itself gets its self-update frame.

### TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers — TARGET-BEHAVIOR, RED → SKIPPED

First run UN-SKIPPED (assertions active):

```
cd /Users/karthyks/workspace/headscale && CGO_ENABLED=1 go test ./hscontrol/mapper/ -run TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers -count=1 -v
```

Observed failure excerpts:

```
Error:      Should not be: "node added"
Messages:   peer 2 must not be queued an 'node added' broadcast for known node 1
Error:      []types.NodeID{0x1} should not contain 0x1
Messages:   peer 3 must not be queued a changed-peers entry for known node 1
Messages:   reconnecting node 1 must only get a targeted/self change, got {Reason:node added TargetNode:0 OriginNode:1 ... PeersChanged:[1] ...}
Messages:   peer 2 received a changed-peers frame for reconnected known node 1
--- FAIL: TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers (0.29s)
```

Confirms the bug at the fan-out layer: the reconnecting existing node's
`change.NodeAdded` broadcast is queued on and delivered to every peer. Test
then skipped with prefix `TARGET-BEHAVIOR (Task 1.5 fix pending): `
(assertions kept intact below the Skip for when the fix lands).

Final status: SKIP

```
=== RUN   TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers
    broadcast_fanout_test.go:167: TARGET-BEHAVIOR (Task 1.5 fix pending): peers receive the 'node added' broadcast
--- SKIP: TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers (0.00s)
```

### TestPatchFanoutRespectsFilterForNode — CHARACTERIZATION, PASS

Command:

```
cd /Users/karthyks/workspace/headscale && CGO_ENABLED=1 go test ./hscontrol/mapper/ -run TestPatchFanoutRespectsFilterForNode -count=1 -v
```

Result: PASS (0.27s). Driven through `addToBatch` with one broadcast patch +
one targeted change (`change.SelfUpdate`, user separation): the non-target
recipient's pending queue holds exactly the broadcast patch and never another
node's targeted change; the target's queue holds both.

### TestOnlinePatchIsCheapSingleBit — MEASUREMENT GUARD, PASS

Command:

```
cd /Users/karthyks/workspace/headscale && CGO_ENABLED=1 go test ./hscontrol/mapper/ -run TestOnlinePatchIsCheapSingleBit -count=1 -v
```

Result: PASS (3.49s). One `change.NodeOnline` patch fanned out to a 50-node
batcher produced exactly 50 responses (one per recipient), each a single-bit
`PeersChangedPatch` (no `Peers`/`PeersChanged` payloads) in **2.41ms** — well
inside the 5s best-effort deadline. This is the canary that regresses if
fan-out starts generating full maps again.

### Package verification

- `CGO_ENABLED=1 go test ./hscontrol/mapper/ -count=1` → ok (228.7s)
- `CGO_ENABLED=1 go test -race ./hscontrol/mapper/ -count=1` → ok (193.8s)

## churn_classification

Date: 2026-08-22 (section reconstructed by parent after a concurrent-write
race between children clobbered the original; results re-verified live).
File: `hscontrol/state/churn_classification_test.go`
Scope: map-request change classification (issue #3417 surface) —
`buildMapRequestChangeResponse` (state.go:3306) and the persist-worthiness
gate (`peerChangePersistWorthy`, state.go:~3383).

### TestEndpointAndDERPChangesProducePatches — CHARACTERIZATION, PASS

Command: `CGO_ENABLED=1 go test ./hscontrol/state/ -run TestEndpointAndDERPChangesProducePatches -count=1 -v`
Result: PASS — endpoint / DERP / combined changes classify as
`"endpoint/DERP update"` patches (single PeerPatch with correct
Endpoints/DERPRegion/NodeID, empty PeersChanged, Type=patch, OriginNode set).

### TestHostinfoChangeShouldNotBroadcastFullUpdate — TARGET-BEHAVIOR, RED → SKIPPED

Observed failure (run un-skipped first): hostinfo-only request returns
`change.NodeAdded(id)` — `Reason:"node added"`, TargetNode unset,
`PeersChanged=[id]`, Type=peers — i.e. a full broadcast to every peer.

### TestNoOpMapRequestProducesNoBroadcast — TARGET-BEHAVIOR, RED → SKIPPED

Observed failure (run un-skipped first): all-false flags falls through to the
final `return change.NodeAdded(id)` (state.go:3335) — same broadcast shape,
`IsEmpty()||IsTargetedToNode()` is false.

### TestLastSeenOnlyRequestSkipsPersistCascade — CHARACTERIZATION, PASS

Result: PASS — table over all 6 persist-worthy fields + end-to-end via
`PeerChangeFromMapRequest`: last_seen-only requests are non-empty but
`peerChangePersistWorthy=false`, so the SetNodes/RebuildPeerMaps cascade is
skipped; key/endpoint/DERP/expiry/online changes are persist-worthy.

### Package verification (parent re-run)

- `go vet ./hscontrol/state/` clean; gofmt clean
- `CGO_ENABLED=1 go test ./hscontrol/state/ -count=1` → ok (48.4s)
- Note (test-side): `tailcfg.PeerChange.DERPRegion` is `int`, not `uint16`.

## hotpath_benches

Date: 2026-08-22. Hardware: Apple M1 Pro, 10 cores, 32 GB RAM.
Go: go1.26.5 darwin/arm64. Baseline = pre-optimization numbers for the
Phase 1/2 before-after gates.

Files:
- `hscontrol/policy/v2/buildpeermap_bench_test.go` — `BenchmarkBuildPeerMap`
  over N × policy shape; `TestBuildPeerMapSymmetry` oracle (PASS).
- `hscontrol/mapper/batcher_scale_bench_test.go` — appended Tier 6:
  `BenchmarkScale_RealMapper_Broadcast1000`, `BenchmarkScale_ReconnectStorm`.

### BenchmarkBuildPeerMap (full -benchtime pass)

```
goos: darwin / goarch: arm64 / cpu: Apple M1 Pro
BenchmarkBuildPeerMap/N=250/policy=allowall-10         250   4.70 ms/op    4.1 MB/op   64507 allocs/op
BenchmarkBuildPeerMap/N=250/policy=acl20-10            124   9.65 ms/op    4.1 MB/op   64507 allocs/op
BenchmarkBuildPeerMap/N=250/policy=autogroupself-10     19  60.2 ms/op    62.0 MB/op  735292 allocs/op
BenchmarkBuildPeerMap/N=500/policy=allowall-10          62  19.8 ms/op   16.7 MB/op  254507 allocs/op
BenchmarkBuildPeerMap/N=500/policy=acl20-10             28  40.1 ms/op   16.7 MB/op  254507 allocs/op
BenchmarkBuildPeerMap/N=500/policy=autogroupself-10      5 239.4 ms/op  249.3 MB/op 2911012 allocs/op
BenchmarkBuildPeerMap/N=1000/policy=allowall-14         14  79.5 ms/op   65.7 MB/op 1010012 allocs/op
BenchmarkBuildPeerMap/N=1000/policy=acl20-10             6 169.8 ms/op   65.7 MB/op 1010012 allocs/op
BenchmarkBuildPeerMap/N=1000/policy=autogroupself-10     1  1.04 s/op   998.1 MB/op 11578017 allocs/op
BenchmarkBuildPeerMap/N=2000/policy=allowall-10          4 316.7 ms/op  288.8 MB/op 4024022 allocs/op
BenchmarkBuildPeerMap/N=2000/policy=acl20-10             2 710.7 ms/op  288.8 MB/op 4024020 allocs/op
BenchmarkBuildPeerMap/N=2000/policy=autogroupself-10     1  4.45 s/op     3.93 GB/op 46168029 allocs/op
```

Reading: quadratic scaling visible in every policy column (~4x per node
doubling). autogroup:self is the worst case by far — a single peer-map rebuild
at N=2000 costs 4.45 s and 3.9 GB of allocation churn, on the single-writer
goroutine that every map request blocks on.

### BenchmarkScale_ReconnectStorm (smoke, -benchtime=1x)

```
BenchmarkScale_ReconnectStorm-10   1   2.30 s/op   8.32 GB/op   110750642 allocs/op
```

64 concurrent AddNode against ~1000 pre-registered conns: 2.3 s wall,
8.3 GB allocated. Note: raw test conns are undrained in this harness, so send
timeouts ("likely stale connection" log lines) are expected noise; the number
is the generation/allocation cost, and it nearly exhausts the real 5 s
initial-map timeout budget in one wave.

### BenchmarkScale_RealMapper_Broadcast1000 (-benchtime=1x)

```
BenchmarkScale_RealMapper_Broadcast1000-10   1   105.6 ms/op   308.2 MB/op   4082906 allocs/op
```

One NodeAdded-style broadcast fanned to 1000 conns with a REAL mapper:
105.6 ms wall per event, 308 MB allocated. Gate number for Tasks 1.1
(no-policy fast path) and 2.1 (per-node visible-peer cache).

### TestBuildPeerMapSymmetry

PASS — N=8 fixture across allow-all / one-way user rule / group rule:
no self-peering, full pairwise symmetry for plain ACL shapes; oracle for the
future incremental peer-map implementation.
