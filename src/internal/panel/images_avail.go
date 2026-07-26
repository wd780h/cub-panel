// Image availability: what a node can actually deliver right now, used to keep
// the storefront from offering images that would fail at provision time.
package panel

import (
	"context"
	"sync"
	"time"

	"cubpanel/internal/store"
)

// availTTL bounds how stale the per-node availability set may get. Image sets
// change only when an admin pulls or deletes one, so minutes are fine and it
// keeps the storefront off the agents on every page view.
const availTTL = 5 * time.Minute

type availEntry struct {
	aliases map[string]bool
	at      time.Time
}

type availCache struct {
	mu sync.Mutex
	m  map[int64]availEntry
}

func newAvailCache() *availCache { return &availCache{m: map[int64]availEntry{}} }

// nodeImages returns the aliases a node can serve: everything cached locally
// plus everything its image server still offers. A node that cannot be reached
// yields nil, which callers treat as "unknown" rather than "nothing".
func (s *Server) nodeImages(ctx context.Context, node *store.Node) map[string]bool {
	s.avail.mu.Lock()
	if e, ok := s.avail.m[node.ID]; ok && time.Since(e.at) < availTTL {
		s.avail.mu.Unlock()
		return e.aliases
	}
	s.avail.mu.Unlock()

	set := map[string]bool{}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	local, lerr := agentImages(cctx, node)
	cancel()
	for _, im := range local {
		for _, a := range im.Aliases {
			set[a] = true
		}
	}
	rctx, rcancel := context.WithTimeout(ctx, 12*time.Second)
	remote, rerr := agentRemoteImages(rctx, node)
	rcancel()
	for _, im := range remote {
		set[im.Alias] = true
	}
	// Both probes failing means the node is unreachable — do not cache an
	// empty set, or a blip would empty the storefront for five minutes.
	if lerr != nil && rerr != nil {
		return nil
	}
	s.avail.mu.Lock()
	s.avail.m[node.ID] = availEntry{aliases: set, at: time.Now()}
	s.avail.mu.Unlock()
	return set
}

// planImages filters a plan's configured aliases down to those a node able to
// host the plan can actually deliver. When availability cannot be determined
// (every candidate node offline) the plan's own list is returned unchanged, so
// a monitoring blip never blanks the storefront.
func (s *Server) planImages(ctx context.Context, plan *store.Plan) []string {
	want := plan.ImageList()
	if len(want) == 0 {
		return nil
	}
	nodes, err := s.db.ListNodes(ctx, true)
	if err != nil || len(nodes) == 0 {
		return want
	}
	union := map[string]bool{}
	known := false
	for _, n := range nodes {
		// Respect the plan's node pin and its capability requirements, so the
		// list matches the node that would actually be scheduled.
		if plan.NodeID > 0 && n.ID != plan.NodeID {
			continue
		}
		if needsV6(plan.Mode) && !n.V6Enabled {
			continue
		}
		if needsDV4(plan.Mode) && !n.V4Enabled {
			continue
		}
		if plan.InstanceType == "vm" && !n.KVMEnabled {
			continue
		}
		if set := s.nodeImages(ctx, n); set != nil {
			known = true
			for a := range set {
				union[a] = true
			}
		}
	}
	if !known {
		return want
	}
	out := make([]string, 0, len(want))
	for _, a := range want {
		if union[a] {
			out = append(out, a)
		}
	}
	return out
}
