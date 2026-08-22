package v2

// Hot-path benchmarks for [PolicyManager.BuildPeerMap] with REAL policies.
//
// Upstream never benchmarked the O(n^2)-ish peer-map rebuild against real
// policy shapes; these benchmarks close that gap and act as the before/after
// gate for the upcoming incremental peer-map work.
//
// Shapes (all with real, parsed policies):
//   - allowall:      single wildcard rule, global-filter fast path.
//   - acl20:         20 realistic rules across 4 users, global-filter path
//                    with a non-uniform visibility graph.
//   - autogroupself: autogroup:member -> autogroup:self:*, per-node filter
//                    slow path (needsPerNodeFilter).
//
// Node fixtures are deterministic: stable IDs 1..N, IPv4 in 100.64.0.0/10,
// IPv6 in fd7a:115c:a1e0::/48 (the same tailnet ranges the db test helpers
// allocate from), hostinfo populated, no tags/routes/exit-node roles so the
// symmetric-ACL oracle in TestBuildPeerMapSymmetry stays in play.

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"tailscale.com/tailcfg"
)

// buildPeerMapBenchCounts is the node-count ladder for the peer-map rebuild.
// 2000 is the practical ceiling: the rebuild is super-linear, and each
// iteration allocates the full peer map.
var buildPeerMapBenchCounts = []int{250, 500, 1000, 2000}

// buildPeerMapBenchUsers returns the 4 deterministic users every shape shares.
func buildPeerMapBenchUsers() types.Users {
	users := make(types.Users, 4)
	for i := range 4 {
		users[i] = types.User{
			Model: gorm.Model{ID: uint(i + 1)}, //nolint:gosec // bounded test index
			Name:  "user" + strconv.Itoa(i+1),
			Email: "user" + strconv.Itoa(i+1) + "@headscale.net",
		}
	}

	return users
}

// buildPeerMapBenchNodes returns N deterministic user-owned nodes spread
// round-robin across the given users. IDs are stable (1..N), addresses live
// in the tailnet ranges and hostinfo is populated like a real registration.
func buildPeerMapBenchNodes(n int, users types.Users) types.Nodes {
	nodes := make(types.Nodes, n)

	for i := range n {
		id := types.NodeID(i + 1)

		// Same layout as db.allocateTestIPs: 100.64.high.low and
		// fd7a:115c:a1e0::high:low with high/low derived from the ID.
		high := byte(id / 256) //nolint:gosec // bounded by the count ladder
		low := byte(id % 256)  //nolint:gosec // bounded by the count ladder

		ipv4 := netip.AddrFrom4([4]byte{100, 64, high, low})
		ipv6 := netip.AddrFrom16([16]byte{
			0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0,
			0, 0, 0, 0, 0, 0, 0, 0,
			high, low,
		})

		user := users[i%len(users)]

		nodes[i] = &types.Node{
			ID:        id,
			Hostname:  "bench-node-" + strconv.Itoa(i+1),
			GivenName: "bench-node-" + strconv.Itoa(i+1),
			IPv4:      &ipv4,
			IPv6:      &ipv6,
			User:      new(user),
			UserID:    new(user.ID),
			Hostinfo: &tailcfg.Hostinfo{
				Hostname: "bench-node-" + strconv.Itoa(i+1),
				OS:       "linux",
				Distro:   "bench",
			},
		}
	}

	return nodes
}

// buildPeerMapACL20Policy generates a policy with 20 realistic ACL rule
// pairs across 4 users: each user owns 5 rules covering common service
// patterns (ssh, web, datastore, dns and a dev port range) aimed at users
// other than itself. The resulting visibility graph is non-uniform, so
// BuildPeerMap has to do real matcher work per pair instead of short-
// circuiting on a wildcard.
func buildPeerMapACL20Policy() []byte {
	type rule struct {
		src  string
		dsts []string
	}

	services := [...][]string{
		{"22"},           // ssh
		{"80", "443"},    // web
		{"5432", "6379"}, // datastore
		{"53"},           // dns
		{"3000-4000"},    // dev range
	}

	email := func(u int) string {
		return "user" + strconv.Itoa(u+1) + "@headscale.net"
	}

	var rules []rule
	for src := range 4 {
		for tier, ports := range services {
			dst := (src + 1 + tier) % 4

			dsts := make([]string, 0, len(ports))
			for _, p := range ports {
				dsts = append(dsts, email(dst)+":"+p)
			}

			rules = append(rules, rule{src: email(src), dsts: dsts})
		}
	}

	var sb strings.Builder
	sb.WriteString(`{"acls":[`)
	for i, r := range rules {
		if i > 0 {
			sb.WriteString(",")
		}

		sb.WriteString(`{"action":"accept","src":["` + r.src + `"],"dst":[`)
		for j, d := range r.dsts {
			if j > 0 {
				sb.WriteString(",")
			}

			sb.WriteString(`"` + d + `"`)
		}
		sb.WriteString("]}")
	}
	sb.WriteString("]}")

	return []byte(sb.String())
}

// buildPeerMapBenchPolicy returns the policy bytes for the named shape.
func buildPeerMapBenchPolicy(name string) []byte {
	switch name {
	case "allowall":
		return []byte(`{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`)

	case "acl20":
		return buildPeerMapACL20Policy()

	case "autogroupself":
		return []byte(`{"acls":[{"action":"accept",` +
			`"src":["autogroup:member"],"dst":["autogroup:self:*"]}]}`)

	default:
		panic("unknown policy shape: " + name)
	}
}

// BenchmarkBuildPeerMap measures the full peer-map rebuild cost across node
// counts and real policy shapes. The manager and node slice are built before
// ResetTimer so the measurement is the rebuild itself, which is the hot path
// re-run on every policy/node change in production.
func BenchmarkBuildPeerMap(b *testing.B) {
	for _, n := range buildPeerMapBenchCounts {
		for _, shape := range []string{"allowall", "acl20", "autogroupself"} {
			b.Run(fmt.Sprintf("N=%d/policy=%s", n, shape), func(b *testing.B) {
				users := buildPeerMapBenchUsers()
				nodes := buildPeerMapBenchNodes(n, users)

				pm, err := NewPolicyManager(
					buildPeerMapBenchPolicy(shape),
					users,
					nodes.ViewSlice(),
				)
				if err != nil {
					b.Fatalf("NewPolicyManager(%s, N=%d): %v", shape, n, err)
				}

				nodeViews := nodes.ViewSlice()

				b.ReportAllocs()
				b.ResetTimer()

				for range b.N {
					_ = pm.BuildPeerMap(nodeViews)
				}
			})
		}
	}
}

// TestBuildPeerMapSymmetry is the characterization oracle the future
// incremental peer-map implementation must match: for plain ACL rules
// (no autogroup:self, no exit nodes, no via grants) the built peer map is
// symmetric — i is in peers(j) iff j is in peers(i) — even when the policy
// itself only grants access one way, because BuildPeerMap deliberately uses
// symmetric visibility (either direction grants both).
func TestBuildPeerMapSymmetry(t *testing.T) {
	users := buildPeerMapBenchUsers()
	nodes := buildPeerMapBenchNodes(8, users)

	// Mixed plain-ACL shapes: a wildcard, a one-way user rule, and a
	// group-based rule. None of them use asymmetric constructs, so the
	// resulting peer map must be fully symmetric in every case.
	shapes := []struct {
		name string
		pol  string
	}{
		{
			name: "allow-all",
			pol:  `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`,
		},
		{
			name: "one-way-acl",
			pol: `{"acls":[{"action":"accept",` +
				`"src":["user1@headscale.net"],` +
				`"dst":["user2@headscale.net:22"]}]}`,
		},
		{
			name: "group-acl",
			pol: `{"groups":{"group:devs":[` +
				`"user1@headscale.net","user2@headscale.net"]},` +
				`"acls":[{"action":"accept",` +
				`"src":["group:devs"],` +
				`"dst":["user3@headscale.net:443"]}]}`,
		},
	}

	// Node ownership for the 8-node fixture: nodes 1-3 user1, 4-6 user2,
	// 7-8 user3 (round robin, same as the generator).
	userNodes := func(u int) []types.NodeID {
		var ids []types.NodeID

		for _, n := range nodes {
			if n.User.ID == users[u].ID {
				ids = append(ids, n.ID)
			}
		}

		return ids
	}

	for _, tt := range shapes {
		t.Run(tt.name, func(t *testing.T) {
			pm, err := NewPolicyManager([]byte(tt.pol), users, nodes.ViewSlice())
			require.NoError(t, err)

			peerMap := pm.BuildPeerMap(nodes.ViewSlice())

			// No node peers with itself.
			for _, n := range nodes {
				for _, peer := range peerMap[n.ID] {
					require.NotEqual(t, n.ID, peer.ID(),
						"node %d must not be its own peer", n.ID)
				}
			}

			// Full symmetry: i in peers(j) iff j in peers(i).
			for _, i := range nodes {
				for _, j := range nodes {
					if i.ID == j.ID {
						continue
					}

					iInJ := false
					for _, peer := range peerMap[j.ID] {
						if peer.ID() == i.ID {
							iInJ = true

							break
						}
					}

					jInI := false
					for _, peer := range peerMap[i.ID] {
						if peer.ID() == j.ID {
							jInI = true

							break
						}
					}

					require.Equal(t, iInJ, jInI,
						"asymmetric peer relationship: %d in peers(%d)=%v, "+
							"%d in peers(%d)=%v",
						i.ID, j.ID, iInJ, j.ID, i.ID, jInI)
				}
			}

			// Characterization of specific shapes, so a future incremental
			// implementation cannot pass symmetry while flipping visibility:
			// under one-way-acl, user1's and user2's nodes must see each
			// other (both directions), and user3's nodes must see nobody.
			if tt.name == "one-way-acl" {
				u1, u2, u3 := userNodes(0), userNodes(1), userNodes(2)

				for _, i := range u1 {
					require.NotEmpty(t, peerMap[i],
						"user1 node %d must see its user2 peers", i)

					for _, peer := range peerMap[i] {
						require.Contains(t, u2, peer.ID(),
							"user1 node %d must only peer with user2 nodes", i)
					}
				}

				for _, i := range u2 {
					require.NotEmpty(t, peerMap[i],
						"user2 node %d must see its user1 peers", i)
				}

				for _, i := range u3 {
					require.Empty(t, peerMap[i],
						"user3 node %d must be isolated", i)
				}
			}
		})
	}
}
