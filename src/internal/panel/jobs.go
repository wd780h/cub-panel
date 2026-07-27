package panel

import (
	"context"
	"log"
	"time"

	"cubpanel/internal/store"
)

// StartJobs launches the periodic maintenance loops. It returns when ctx is
// cancelled; each loop runs on its own goroutine.
func (s *Server) StartJobs(ctx context.Context) {
	go s.loop(ctx, 2*time.Minute, s.probeNodes)
	go s.loop(ctx, 5*time.Minute, s.reapExpired)
	go s.loop(ctx, 5*time.Minute, s.meterTraffic)
	go s.loop(ctx, time.Hour, s.purgeSessions)
	go s.loop(ctx, time.Hour, s.purgeEmailVerifications)
}

// loop runs fn immediately and then on a fixed interval.
func (s *Server) loop(ctx context.Context, every time.Duration, fn func(context.Context)) {
	fn(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// probeNodes health-checks every enabled node and records the outcome, which
// is what drives the online badges in the UI.
func (s *Server) probeNodes(ctx context.Context) {
	nodes, err := s.db.ListNodes(ctx, true)
	if err != nil {
		return
	}
	for _, n := range nodes {
		c, cancel := context.WithTimeout(ctx, 12*time.Second)
		if _, err := agentHealth(c, n); err != nil {
			_ = s.db.TouchNode(ctx, n.ID, "error: "+err.Error())
		} else {
			_ = s.db.TouchNode(ctx, n.ID, "ok")
		}
		cancel()
	}
}

// reapExpired stops containers whose term has run out. Data is kept so an
// administrator can still renew; only the running process is reclaimed.
func (s *Server) reapExpired(ctx context.Context) {
	list, err := s.db.ExpiredInstances(ctx)
	if err != nil || len(list) == 0 {
		return
	}
	for _, inst := range list {
		node, err := s.db.NodeByID(ctx, inst.NodeID)
		if err != nil {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := agentAction(c, node, inst.Name, "stop", true); err != nil {
			log.Printf("reap %s: %v", inst.Name, err)
		}
		cancel()
		_ = s.db.SetInstanceStatus(ctx, inst.ID, "expired", "已到期，已自动停机")
		s.db.Audit(ctx, inst.UserID, "system", "instance.expire", inst.Name, "")
	}
}

// meterTraffic samples every instance's byte counters, accumulates deltas
// (the node counters reset when a container restarts), applies the monthly
// zeroing, and stops instances that blow through their allowance.
func (s *Server) meterTraffic(ctx context.Context) {
	list, err := s.db.ListInstances(ctx, 0)
	if err != nil {
		return
	}
	nodes := map[int64]*store.Node{}
	now := time.Now().Unix()

	for _, inst := range list {
		if inst.Status != "running" {
			continue
		}
		node, ok := nodes[inst.NodeID]
		if !ok {
			if node, err = s.db.NodeByID(ctx, inst.NodeID); err != nil {
				continue
			}
			nodes[inst.NodeID] = node
		}
		c, cancel := context.WithTimeout(ctx, 12*time.Second)
		st, err := agentState(c, node, inst.Name)
		cancel()
		if err != nil {
			continue
		}

		usedRX := inst.UsedRX + counterDelta(st.NetRx, inst.LastRX)
		usedTX := inst.UsedTX + counterDelta(st.NetTx, inst.LastTX)

		// Monthly zeroing.
		if inst.TrafficResetAt > 0 && now >= inst.TrafficResetAt {
			usedRX, usedTX = 0, 0
			next := inst.TrafficResetAt
			for next <= now {
				next += 30 * 86400
			}
			_ = s.db.ResetTraffic(ctx, inst.ID, next)
		}
		_ = s.db.UpdateTrafficCounters(ctx, inst.ID, usedRX, usedTX, st.NetRx, st.NetTx)

		// Enforcement.
		if lim := inst.TrafficLimitBytes(); lim > 0 {
			var used int64
			switch inst.TrafficMode {
			case "up":
				used = usedTX
			case "down":
				used = usedRX
			default:
				used = usedRX + usedTX
			}
			if used >= lim {
				c, cancel := context.WithTimeout(ctx, 60*time.Second)
				if err := agentAction(c, node, inst.Name, "stop", true); err != nil {
					log.Printf("overquota stop %s: %v", inst.Name, err)
				}
				cancel()
				_ = s.db.SetInstanceStatus(ctx, inst.ID, "overquota", "流量超限，已自动停机")
				s.db.Audit(ctx, inst.UserID, "system", "instance.overquota", inst.Name, "")
			}
		}
	}
}

// counterDelta handles the node counters resetting to zero on reboot.
func counterDelta(cur, last int64) int64 {
	if cur >= last {
		return cur - last
	}
	return cur
}

// purgeSessions drops expired session rows.
func (s *Server) purgeSessions(ctx context.Context) {
	_ = s.db.PurgeSessions(ctx)
}

// purgeEmailVerifications drops expired registration challenges.
func (s *Server) purgeEmailVerifications(ctx context.Context) {
	_ = s.db.PurgeExpiredEmailVerifications(ctx)
}
