package state

import (
	"net/netip"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/types/change"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// isBroadcastPeersChanged reports whether c fans a whole-node update out to
// every peer of the node: a PeersChanged list with no target scoping. This is
// the escalation documented in
// https://github.com/juanfont/headscale/issues/3417: routine client chatter is
// classified so that every peer receives and reconciles the node's entry,
// many times per connection interval, which blows up CPU at scale.
func isBroadcastPeersChanged(c change.Change) bool {
	return !c.IsTargetedToNode() && len(c.PeersChanged) > 0
}

// TestEndpointAndDERPChangesProducePatches characterises the current
// classification in [buildMapRequestChangeResponse]: endpoint and/or
// DERP-region deltas are classified as single-peer PeersChanged updates —
// authoritative whole-node entries for just the changed node, which clients
// apply to their WireGuard configuration (a bare PeerChange patch updates
// ipnstate but never re-attaches a peer to a DERP relay).
func TestEndpointAndDERPChangesProducePatches(t *testing.T) {
	ep1 := netip.MustParseAddrPort("100.64.0.1:41641")
	ep2 := netip.MustParseAddrPort("203.0.113.7:41641")

	tests := []struct {
		name            string
		node            types.Node
		endpointChanged bool
		derpChanged     bool
	}{
		{
			name: "endpoint-change-yields-peer-update",
			node: types.Node{
				ID:        5,
				Endpoints: []netip.AddrPort{ep1, ep2},
			},
			endpointChanged: true,
		},
		{
			name: "derp-change-yields-patch-carrying-derpregion",
			node: types.Node{
				ID: 6,
				Hostinfo: &tailcfg.Hostinfo{
					NetInfo: &tailcfg.NetInfo{PreferredDERP: 3},
				},
			},
			derpChanged: true,
		},
		{
			name: "combined-endpoint-and-derp-change-carries-both",
			node: types.Node{
				ID:        7,
				Endpoints: []netip.AddrPort{ep1},
				Hostinfo: &tailcfg.Hostinfo{
					NetInfo: &tailcfg.NetInfo{PreferredDERP: 4},
				},
			},
			endpointChanged: true,
			derpChanged:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := buildMapRequestChangeResponse(
				tt.node.ID, tt.node.View(), false, tt.endpointChanged, tt.derpChanged)
			require.NoError(t, err)

			// Endpoint/DERP deltas ride a single-peer authoritative update...
			require.Equal(t, []types.NodeID{tt.node.ID}, ch.PeersChanged)

			// ...scoped to exactly the changed node: never the #3417
			// NodeAdded escalation (whole-node update classified as "node
			// added" and fanned out at connection rate).
			assert.Empty(t, ch.PeerPatches)
			assert.False(t, ch.IsNodeAdded(),
				"endpoint/DERP deltas must be scoped single-peer updates, not NodeAdded")
			assert.Equal(t, "peers", ch.Type())
			assert.Equal(t, "endpoint/DERP update", ch.Reason)
		})
	}
}

// TestHostinfoCombinedWithEndpointOrDERPChangeStillAdvertisesToPeers pins the
// follow-up regression fix for #3417: a map request that changed BOTH hostinfo
// and endpoint/DERP state must still be classified as a single-peer
// PeersChanged update so peers learn the new endpoints / DERP region.
// Classifying it as a targeted self-update drops the advertisement entirely —
// peers keep peer.Relay == "" and fail DERP sync (integration:
// TestPingAllByIP). The requesting node is not short-changed either: any
// change carrying [Change.OriginNode] is routed back to the origin as a
// self-update by the mapper/batcher.
func TestHostinfoCombinedWithEndpointOrDERPChangeStillAdvertisesToPeers(t *testing.T) {
	ep := netip.MustParseAddrPort("203.0.113.7:41641")

	tests := []struct {
		name string
		node types.Node
	}{
		{
			name: "hostinfo-plus-derp-change",
			node: types.Node{
				ID: 21,
				Hostinfo: &tailcfg.Hostinfo{
					Hostname: "combined-derp",
					NetInfo:  &tailcfg.NetInfo{PreferredDERP: 2},
				},
			},
		},
		{
			name: "hostinfo-plus-endpoint-change",
			node: types.Node{
				ID:        22,
				Endpoints: []netip.AddrPort{ep},
				Hostinfo:  &tailcfg.Hostinfo{Hostname: "combined-endpoint"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := buildMapRequestChangeResponse(tt.node.ID, tt.node.View(), true, true, true)
			require.NoError(t, err)

			// Peers must receive an authoritative single-peer update, never
			// just a targeted self-update or a bare patch.
			require.Equalf(t, []types.NodeID{tt.node.ID}, ch.PeersChanged,
				"combined hostinfo+endpoint/DERP change must advertise the node to peers so they learn the relay/endpoints, got: %#v", ch)
			assert.Empty(t, ch.PeerPatches)
			assert.Equal(t, "peers", ch.Type())
			assert.False(t, ch.IsNodeAdded(),
				"existing-node map requests must never be classified as NodeAdded")
		})
	}
}

// TestHostinfoChangeShouldNotBroadcastFullUpdate pins the target behaviour for
// https://github.com/juanfont/headscale/issues/3417: a hostinfo-only map
// request (service lists, version strings and other routine client chatter)
// must be classified as a lightweight change scoped to the affected node, not
// escalated to NodeAdded, which fans a whole-node update out to every peer.
//
// On current code this fails: buildMapRequestChangeResponse returns
// change.NodeAdded(id) for any hostinfoChanged.
func TestHostinfoChangeShouldNotBroadcastFullUpdate(t *testing.T) {
	node := types.Node{ID: 9}

	ch, err := buildMapRequestChangeResponse(node.ID, node.View(), true, false, false)
	require.NoError(t, err)

	// The hostinfo delta must stay lightweight: targeted at the affected
	// node or carried as a patch, never broadcast to every peer.
	require.Falsef(t, isBroadcastPeersChanged(ch),
		"hostinfo-only change must not broadcast a whole-node update to "+
			"every peer, got: %#v", ch)
}

// TestNoOpMapRequestProducesNoBroadcast pins the target behaviour for
// https://github.com/juanfont/headscale/issues/3417: a map request that
// changed nothing material must not wake up any peer. It should classify as
// an empty change (which the mapper drops) or, at most, a targeted response.
//
// On current code this fails: even the all-false fallback path returns
// change.NodeAdded(id).
func TestNoOpMapRequestProducesNoBroadcast(t *testing.T) {
	node := types.Node{ID: 11}

	ch, err := buildMapRequestChangeResponse(node.ID, node.View(), false, false, false)
	require.NoError(t, err)

	// Nothing changed, so nobody should be notified: an empty change or a
	// targeted one at most, never a broadcast.
	require.Truef(t, ch.IsEmpty() || ch.IsTargetedToNode(),
		"no-op map request must not produce a broadcast change, got: %#v", ch)
}

// TestLastSeenOnlyRequestSkipsPersistCascade characterises the classification
// that keeps last_seen-only map requests cheap: PeerChangeFromMapRequest
// always stamps LastSeen, but peerChangePersistWorthy deliberately ignores it,
// so a keepalive request is classified persist-unworthy and skips the
// persistNodeToDB cascade (full-row UPDATE plus the O(n) policy SetNodes scan
// that rebuilds peer maps) on the hot map-request path.
func TestLastSeenOnlyRequestSkipsPersistCascade(t *testing.T) {
	now := time.Now()
	expiry := now.Add(time.Hour)
	nodePub := key.NewNode().Public()
	discoPub := key.NewDisco().Public()
	online := true

	tests := []struct {
		name       string
		peerChange tailcfg.PeerChange
		want       bool
	}{
		{
			name:       "empty-peer-change-is-persist-unworthy",
			peerChange: tailcfg.PeerChange{},
			want:       false,
		},
		{
			name:       "lastseen-only-is-persist-unworthy",
			peerChange: tailcfg.PeerChange{LastSeen: &now},
			want:       false,
		},
		{
			name:       "node-key-change-is-persist-worthy",
			peerChange: tailcfg.PeerChange{Key: &nodePub},
			want:       true,
		},
		{
			name:       "disco-key-change-is-persist-worthy",
			peerChange: tailcfg.PeerChange{DiscoKey: &discoPub},
			want:       true,
		},
		{
			name:       "online-flip-is-persist-worthy",
			peerChange: tailcfg.PeerChange{Online: &online},
			want:       true,
		},
		{
			name: "endpoint-change-is-persist-worthy",
			peerChange: tailcfg.PeerChange{
				Endpoints: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.7:41641")},
			},
			want: true,
		},
		{
			name:       "derp-region-change-is-persist-worthy",
			peerChange: tailcfg.PeerChange{DERPRegion: 2},
			want:       true,
		},
		{
			name:       "key-expiry-change-is-persist-worthy",
			peerChange: tailcfg.PeerChange{KeyExpiry: &expiry},
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerChangePersistWorthy(tt.peerChange)
			assert.Equal(t, tt.want, got)
		})
	}

	// End-to-end over the map-request path: a keepalive MapRequest that
	// differs from stored state only by LastSeen produces exactly such a
	// persist-unworthy PeerChange.
	node := createTestNode(13, 1, "test-user", "lastseen-node")
	req := tailcfg.MapRequest{NodeKey: node.NodeKey, DiscoKey: node.DiscoKey}
	pc := node.PeerChangeFromMapRequest(req)

	require.NotNil(t, pc.LastSeen, "PeerChangeFromMapRequest always stamps LastSeen")
	assert.Nil(t, pc.Key)
	assert.Nil(t, pc.DiscoKey)
	assert.Nil(t, pc.Endpoints)
	assert.Zero(t, pc.DERPRegion)

	// LastSeen alone keeps the peer change non-empty, so the
	// peerChangeEmpty early return in UpdateNodeFromMapRequest does not
	// fire. It is the persistWorthy classification below that skips
	// persistNodeToDB and its SetNodes/RebuildPeerMaps cascade.
	assert.False(t, peerChangeEmpty(pc))
	assert.False(t, peerChangePersistWorthy(pc),
		"last_seen-only map requests must be classified persist-unworthy")
}
