package mapper

import (
	"net/netip"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/types/change"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
)

// This file pins the batcher's broadcast fan-out mechanics: who receives what
// kind of change once a change enters [Batcher.AddWork]. Per-recipient payload
// visibility (ACL filtering of incremental vs full-map responses) is covered
// by TestBuildFromChangeVisibilityMatchesFullMap; the tests here are about the
// fan-out topology itself — broadcast vs targeted routing, per-connection
// queue contents, and the cost shape of single-bit patches.
//
// Upstream context: juanfont/headscale#3417 — on v0.29, reconnecting an
// EXISTING long-known node emits "node added"-reason changes that fan full
// netmap rebuilds out to every peer at connection rate.

// pendingChangesSnapshot returns a copy of the changes currently queued for a
// connection without draining them.
func pendingChangesSnapshot(nc *multiChannelNodeConn) []change.Change {
	nc.pendingMu.Lock()
	defer nc.pendingMu.Unlock()

	return append([]change.Change(nil), nc.pending...)
}

// drainMapResponses consumes whatever is currently buffered on ch without
// blocking.
func drainMapResponses(ch <-chan *tailcfg.MapResponse) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// scanMapResponses reports whether any frame currently buffered on ch matches,
// consuming buffered frames as it scans. Used inside EventuallyWithT so a
// match appearing on any retry iteration is observed.
func scanMapResponses(ch <-chan *tailcfg.MapResponse, match func(*tailcfg.MapResponse) bool) bool {
	matched := false

	for {
		select {
		case resp := <-ch:
			if match(resp) {
				matched = true
			}
		default:
			return matched
		}
	}
}

// changedPeersContain reports whether resp carries a PeersChanged entry for
// want.
func changedPeersContain(resp *tailcfg.MapResponse, want types.NodeID) bool {
	if resp == nil {
		return false
	}

	for _, peer := range resp.PeersChanged {
		if peer.ID == want.NodeID() {
			return true
		}
	}

	return false
}

// quiesceBatcher deterministically drives [Batcher.processBatchedChanges]
// until every connection has an empty pending queue and no bundle in flight.
// inFlight is only cleared after the worker has applied (sent) the whole
// bundle, so once this returns all generated frames are already buffered on
// the node channels — no sleeping, no reliance on the batch tick.
func quiesceBatcher(t *testing.T, batcher *Batcher) {
	t.Helper()

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		batcher.processBatchedChanges()

		batcher.nodes.Range(func(id types.NodeID, nc *multiChannelNodeConn) bool {
			if nc == nil {
				return true
			}

			pending := pendingChangesSnapshot(nc)
			assert.Emptyf(c, pending, "node %d still has pending changes: %v", id, pending)
			assert.Falsef(c, nc.inFlight.Load(), "node %d still has a bundle in flight", id)

			return true
		})
	}, updateTimeout, 25*time.Millisecond)
}

// TestNewNodeAdditionReachesAllPeers characterizes the fan-out of
// change.NodeAdded for a genuinely NEW node: it is a broadcast, so every
// registered peer connection must eventually receive a changed-peers payload
// containing the new node. The added node itself receives its self-update
// instead ([change.Change.OriginNode] routes it to the self-map path).
func TestNewNodeAdditionReachesAllPeers(t *testing.T) {
	testData, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, 3, 1, normalBufferSize)
	defer cleanup()

	batcher := testData.Batcher
	lfb := unwrapBatcher(batcher)
	nodes := testData.Nodes

	// Only the two established peers connect here; the "new" node joins the
	// batcher below via its NodeAdded broadcast. The initial maps of the two
	// established peers are generated from the NodeStore snapshot, which in
	// this fixture already contains node 3 — so we clear lastSentPeers for
	// it to model a genuinely new registration that peers have never seen.
	for i := range nodes[:2] {
		n := &nodes[i]
		require.NoError(t, batcher.AddNode(n.n.ID, n.ch, tailcfg.CapabilityVersion(100), nil))
	}

	// Flush the registration-time online notifications deterministically and
	// clear the initial maps so only the NodeAdded fan-out is observed below.
	quiesceBatcher(t, lfb)
	for i := range nodes[:2] {
		drainMapResponses(nodes[i].ch)

		nc, ok := lfb.nodes.Load(nodes[i].n.ID)
		require.True(t, ok)
		nc.lastSentPeers.Delete(nodes[2].n.ID.NodeID())
	}

	newNode := &nodes[2]
	// The new node's poll connection registers with the batcher before the
	// NodeAdded broadcast lands (matching poll.go ordering: AddNode precedes
	// Change), so its targeted self-update has a destination.
	require.NoError(t, batcher.AddNode(newNode.n.ID, newNode.ch, tailcfg.CapabilityVersion(100), nil))
	drainMapResponses(newNode.ch)
	batcher.AddWork(change.NodeAdded(newNode.n.ID))
	lfb.processBatchedChanges()

	for i := range nodes[:2] {
		peer := &nodes[i]

		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			received := scanMapResponses(peer.ch, func(resp *tailcfg.MapResponse) bool {
				return changedPeersContain(resp, newNode.n.ID)
			})
			assert.Truef(c, received,
				"peer %d must receive a changed-peers payload containing new node %d",
				peer.n.ID, newNode.n.ID)
		}, updateTimeout, 25*time.Millisecond)
	}

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		selfUpdate := scanMapResponses(newNode.ch, func(resp *tailcfg.MapResponse) bool {
			return resp != nil && resp.Node != nil
		})
		assert.Truef(c, selfUpdate,
			"added node %d must receive its own self-update frame", newNode.n.ID)
	}, updateTimeout, 25*time.Millisecond)
}

// TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers is a target-behavior
// test for upstream issue #3417: reconnecting an EXISTING long-known node must
// not fan an "node added"-reason broadcast to every peer (today that triggers
// full netmap rebuilds at 8-14x connection rate). The fixed behavior is that
// peers' fan-out queues stay free of "node added" broadcasts while the
// reconnecting node itself receives only a targeted/self change.
func TestReconnectOfExistingNodeDoesNotEmitNodeAddedToPeers(t *testing.T) {
	// TARGET-BEHAVIOR (Task 1.5 fix pending): observed 2026-08-22 — both peers
	// get the 'node added' broadcast queued (PeersChanged:[reconnecting]) and
	// delivered, and the reconnecting node itself gets an untargeted broadcast.

	testData, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, 3, 1, normalBufferSize)
	defer cleanup()

	batcher := testData.Batcher
	lfb := unwrapBatcher(batcher)
	nodes := testData.Nodes

	for i := range nodes {
		n := &nodes[i]
		require.NoError(t, batcher.AddNode(n.n.ID, n.ch, tailcfg.CapabilityVersion(100), nil))
	}

	quiesceBatcher(t, lfb)
	for i := range nodes {
		drainMapResponses(nodes[i].ch)
	}

	// Freeze the batch tick so nothing drains or delivers behind our back:
	// from here on the fan-out is inspected and driven deterministically.
	lfb.tick.Stop()

	// An established tailnet member reconnects; today state.go escalates the
	// resulting hostinfo/relogin change to NodeAdded. Feed exactly that change
	// through the public entry point and inspect who it fans out to.
	reconnecting := &nodes[0]
	lfb.AddWork(change.NodeAdded(reconnecting.n.ID))

	for _, peer := range []*node{&nodes[1], &nodes[2]} {
		peerNC, ok := lfb.nodes.Load(peer.n.ID)
		require.Truef(t, ok, "peer %d must be registered with the batcher", peer.n.ID)

		for _, queued := range pendingChangesSnapshot(peerNC) {
			assert.NotEqualf(t, "node added", queued.Reason,
				"peer %d must not be queued an 'node added' broadcast for known node %d",
				peer.n.ID, reconnecting.n.ID)
			assert.NotContainsf(t, queued.PeersChanged, reconnecting.n.ID,
				"peer %d must not be queued a changed-peers entry for known node %d",
				peer.n.ID, reconnecting.n.ID)
		}
	}

	// The reconnecting node itself must still be told something (a targeted or
	// self-shaped change), never silently dropped.
	reconNC, ok := lfb.nodes.Load(reconnecting.n.ID)
	require.Truef(t, ok, "reconnecting node %d must be registered", reconnecting.n.ID)

	if !ok {
		return
	}

	queuedForSelf := pendingChangesSnapshot(reconNC)
	require.NotEmpty(t, queuedForSelf, "reconnecting node must receive a change")

	for _, queued := range queuedForSelf {
		isTargetedAtSelf := queued.TargetNode == reconnecting.n.ID
		assert.Truef(t, isTargetedAtSelf || queued.IsSelfOnly(),
			"reconnecting node %d must only get a targeted/self change, got %+v",
			reconnecting.n.ID, queued)
	}

	// Delivery level: after the deterministic drain, no peer channel may carry
	// a changed-peers frame for the reconnected node.
	lfb.processBatchedChanges()
	quiesceBatcher(t, lfb)

	for _, peer := range []*node{&nodes[1], &nodes[2]} {
		got := scanMapResponses(peer.ch, func(resp *tailcfg.MapResponse) bool {
			return changedPeersContain(resp, reconnecting.n.ID)
		})
		assert.Falsef(t, got,
			"peer %d received a changed-peers frame for reconnected known node %d",
			peer.n.ID, reconnecting.n.ID)
	}
}

// TestPatchFanoutRespectsFilterForNode characterizes addToBatch's per-node
// filtering: a broadcast patch is appended to every registered connection's
// pending queue via change.FilterForNode, while a change carrying a
// TargetNode is routed ONLY to that node's connection — a recipient never has
// another node's targeted change in its pending queue. User separation gives
// the "cannot see" framing here (each node belongs to a distinct user); note
// the batcher-level gate is TargetNode-based — policy visibility of payload
// contents happens later at response-build time and is covered by the oracle
// visibility test.
func TestPatchFanoutRespectsFilterForNode(t *testing.T) {
	testData, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, 3, 1, normalBufferSize)
	defer cleanup()

	batcher := testData.Batcher
	lfb := unwrapBatcher(batcher)
	nodes := testData.Nodes

	for i := range nodes {
		n := &nodes[i]
		require.NoError(t, batcher.AddNode(n.n.ID, n.ch, tailcfg.CapabilityVersion(100), nil))
	}

	quiesceBatcher(t, lfb)
	for i := range nodes {
		drainMapResponses(nodes[i].ch)
	}

	lfb.tick.Stop()

	// recipient = user1's node, patchedPeer = user2's node, target = user3's
	// node: three distinct users under the helper's allow-all policy.
	recipient := &nodes[0]
	patchedPeer := &nodes[1]
	target := &nodes[2]

	broadcastPatch := change.PeerPatched("endpoint/DERP update", &tailcfg.PeerChange{
		NodeID:    patchedPeer.n.ID.NodeID(),
		Endpoints: []netip.AddrPort{netip.MustParseAddrPort("100.64.0.10:41641")},
	})
	targetedPatch := change.SelfUpdate(target.n.ID)

	lfb.addToBatch(broadcastPatch, targetedPatch)

	// The recipient gets exactly the broadcast patch; the targeted change for
	// user3's node must not appear in its pending queue.
	recipientNC, ok := lfb.nodes.Load(recipient.n.ID)
	require.Truef(t, ok, "recipient %d must be registered", recipient.n.ID)

	recipientPending := pendingChangesSnapshot(recipientNC)
	require.Lenf(t, recipientPending, 1,
		"recipient %d pending queue must hold only the broadcast patch", recipient.n.ID)
	assert.Zerof(t, recipientPending[0].TargetNode,
		"recipient %d must not be queued another node's targeted change", recipient.n.ID)

	// The target gets both its targeted change and the broadcast patch.
	targetNC, ok := lfb.nodes.Load(target.n.ID)
	require.Truef(t, ok, "target %d must be registered", target.n.ID)

	targetPending := pendingChangesSnapshot(targetNC)
	foundTargeted := false

	for _, queued := range targetPending {
		if queued.TargetNode == target.n.ID && queued.Reason == "self update" {
			foundTargeted = true
		}
	}

	assert.Truef(t, foundTargeted,
		"target %d must have the targeted change queued, got %+v", target.n.ID, targetPending)

	lfb.processBatchedChanges()
	quiesceBatcher(t, lfb)

	// Delivery level: the recipient's channel carries the broadcast patch and
	// never a self-update for another user's node.
	gotPatch := scanMapResponses(recipient.ch, func(resp *tailcfg.MapResponse) bool {
		if resp == nil {
			return false
		}

		for _, patch := range resp.PeersChangedPatch {
			if patch.NodeID == patchedPeer.n.ID.NodeID() {
				return true
			}
		}

		return false
	})
	assert.Truef(t, gotPatch,
		"recipient %d must receive the broadcast endpoint patch", recipient.n.ID)

	gotForeignSelf := scanMapResponses(recipient.ch, func(resp *tailcfg.MapResponse) bool {
		return resp != nil && resp.Node != nil
	})
	assert.Falsef(t, gotForeignSelf,
		"recipient %d must not receive a self-update frame for another node", recipient.n.ID)
}

// TestOnlinePatchIsCheapSingleBit is the measurement guard for #3417-style
// regressions: a boolean online patch fanned out to a 50-node batcher must
// produce exactly one small response per connected recipient — never full-map
// payloads — and do so within a generous wall-clock budget. If fan-out ever
// starts generating netmap rebuilds again, the payload-shape assertions fail.
func TestOnlinePatchIsCheapSingleBit(t *testing.T) {
	const fanoutNodes = 50

	// fanoutDeadline bounds the whole delivery; deliberately generous because
	// CI machines vary. The timing assertion is best-effort; the payload-shape
	// assertions below are the hard contract.
	const fanoutDeadline = 5 * time.Second

	testData, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, fanoutNodes, 1, normalBufferSize)
	defer cleanup()

	lfb := unwrapBatcher(testData.Batcher)
	st := testData.State
	nodes := testData.Nodes

	// Register directly on the raw batcher (no wrapper) so registration does
	// not enqueue per-node online broadcasts that would pollute the counts.
	for i := range nodes {
		n := &nodes[i]
		st.Connect(n.n.ID)
		require.NoError(t, lfb.AddNode(n.n.ID, n.ch, tailcfg.CapabilityVersion(100), nil))
	}

	// Clear the initial maps; freeze the tick and drive the pipeline manually
	// so exactly one fan-out is measured.
	for i := range nodes {
		drainMapResponses(nodes[i].ch)
	}

	lfb.tick.Stop()

	patched := &nodes[0]
	start := time.Now()

	lfb.AddWork(change.NodeOnline(patched.n.ID))
	lfb.processBatchedChanges()

	responses := 0

	for i := range nodes {
		n := &nodes[i]

		remaining := fanoutDeadline - time.Since(start)
		if remaining <= 0 {
			t.Fatalf(
				"online-patch fan-out to %d nodes blew the %s deadline after %d/%d responses",
				fanoutNodes, fanoutDeadline, responses, fanoutNodes)
		}

		select {
		case resp := <-n.ch:
			responses++
			require.NotNil(t, resp)

			// Hard payload-shape assertion: single-bit patches ride
			// PeersChangedPatch; a Peers/PeersChanged payload here means a
			// full map or peer-list rebuild was fanned out again.
			assert.Emptyf(t, resp.Peers,
				"node %d received a full-map payload for a boolean patch", n.n.ID)
			assert.Emptyf(t, resp.PeersChanged,
				"node %d received a changed-peers payload for a boolean patch", n.n.ID)

			if n.n.ID == patched.n.ID {
				continue
			}

			require.Lenf(t, resp.PeersChangedPatch, 1,
				"node %d must receive exactly one single-bit patch", n.n.ID)
			patch := resp.PeersChangedPatch[0]
			assert.Equalf(t, patched.n.ID.NodeID(), patch.NodeID,
				"node %d got a patch for the wrong peer", n.n.ID)
			require.NotNil(t, patch.Online)
			assert.True(t, *patch.Online)
		case <-time.After(remaining):
			t.Fatalf(
				"node %d never received the online patch within the deadline (%d/%d delivered)",
				n.n.ID, responses, fanoutNodes)
		}
	}

	elapsed := time.Since(start)
	t.Logf("online patch fanned out to %d nodes in %s", responses, elapsed)

	// Best-effort timing guard (CI variance); the shape assertions above are
	// the real canary.
	assert.LessOrEqualf(t, elapsed, fanoutDeadline,
		"online-patch fan-out took %s, over the generous %s budget", elapsed, fanoutDeadline)
	assert.Equal(t, fanoutNodes, responses,
		"one MapResponse per connected recipient expected")
}
