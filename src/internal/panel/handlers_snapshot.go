// Snapshot management (tenant) and cross-node migration (admin).
package panel

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cubpanel/internal/store"
)

// maxSnapshotsPerInstance caps how many snapshots a tenant may keep.
const maxSnapshotsPerInstance = 3

var snapNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,31}$`)

// handleAPISnapshotList returns an instance's snapshots.
func (s *Server) handleAPISnapshotList(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	snaps, err := agentSnapshots(ctx, node, inst.Name)
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, "读取快照失败："+err.Error())
		return
	}
	s.jsonOK(w, map[string]any{"snapshots": snaps})
}

// handleAPISnapshotCreate takes a new snapshot (up to the per-instance cap).
func (s *Server) handleAPISnapshotCreate(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	snap := formStr(r, "snapshot", 32)
	if !snapNameRe.MatchString(snap) {
		s.jsonErr(w, http.StatusBadRequest, "快照名只能含字母、数字与 . _ -")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Minute)
	defer cancel()

	if cur, err := agentSnapshots(ctx, node, inst.Name); err == nil && len(cur) >= maxSnapshotsPerInstance {
		s.jsonErr(w, http.StatusConflict, fmt.Sprintf("每台实例最多保留 %d 个快照，请先删除旧快照", maxSnapshotsPerInstance))
		return
	}
	if err := agentSnapshotCreate(ctx, node, inst.Name, snap); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "创建快照失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "snapshot.create", inst.Name+"@"+snap, clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true})
}

// handleAPISnapshotRestore rolls the instance back.
func (s *Server) handleAPISnapshotRestore(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	snap := formStr(r, "snapshot", 32)
	if !snapNameRe.MatchString(snap) {
		s.jsonErr(w, http.StatusBadRequest, "快照名无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Minute)
	defer cancel()
	if err := agentSnapshotRestore(ctx, node, inst.Name, snap); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "还原失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "snapshot.restore", inst.Name+"@"+snap, clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true})
}

// handleAPISnapshotDelete removes a snapshot.
func (s *Server) handleAPISnapshotDelete(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	snap := formStr(r, "snapshot", 32)
	if !snapNameRe.MatchString(snap) {
		s.jsonErr(w, http.StatusBadRequest, "快照名无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := agentSnapshotDelete(ctx, node, inst.Name, snap); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "删除失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "snapshot.delete", inst.Name+"@"+snap, clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true})
}

// ---------- migration (admin) ----------

// handleAdminInstanceMigrate cold-migrates an instance to another node. The
// source is only destroyed after the destination is verified running, so a
// failure at any step leaves the original intact.
func (s *Server) handleAdminInstanceMigrate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inst, err := s.db.InstanceByID(ctx, formInt64(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	src, err := s.db.NodeByID(ctx, inst.NodeID)
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "源节点不可用")
		return
	}
	dst, err := s.db.NodeByID(ctx, formInt64(r, "dest_node"))
	if err != nil || !dst.Enabled {
		s.renderError(w, r, http.StatusBadRequest, "目标节点无效或已停用")
		return
	}
	if dst.ID == src.ID {
		s.renderError(w, r, http.StatusBadRequest, "目标节点与当前节点相同")
		return
	}
	// The instance backup only carries the root disk; custom data volumes
	// would be silently left behind, so refuse rather than lose data.
	if strings.TrimSpace(inst.ExtraDisks) != "" {
		s.renderError(w, r, http.StatusBadRequest, "带附加数据盘的实例暂不支持跨节点迁移")
		return
	}
	if needsV6(inst.Mode) && !dst.V6Enabled {
		s.renderError(w, r, http.StatusBadRequest, "目标节点未开启独立 IPv6")
		return
	}
	if needsDV4(inst.Mode) && !dst.V4Enabled {
		s.renderError(w, r, http.StatusBadRequest, "目标节点未开启独立公网 IPv4")
		return
	}
	if inst.InstanceType == "vm" && !dst.KVMEnabled {
		s.renderError(w, r, http.StatusBadRequest, "目标节点未开启 KVM")
		return
	}

	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.migrate.start",
		fmt.Sprintf("%s: %s → %s", inst.Name, src.Name, dst.Name), clientIP(r))

	// Migration copies a full disk image; run it detached and report via the
	// instance status + audit log.
	_ = s.db.SetInstanceStatus(ctx, inst.ID, "migrating", "正在迁移到 "+dst.Name)
	go s.runMigration(inst, src, dst)

	http.Redirect(w, r, "/admin/instances", http.StatusSeeOther)
}

// runMigration performs the cold migration end to end.
func (s *Server) runMigration(inst *store.Instance, src, dst *store.Node) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	fail := func(stage string, err error) {
		s.db.Audit(ctx, 0, "system", "instance.migrate.fail",
			fmt.Sprintf("%s at %s: %v", inst.Name, stage, err), "")
		// Best effort: restart the still-intact source and clean the dest.
		_ = agentAction(ctx, src, inst.Name, "start", false)
		_ = agentBackupDelete(ctx, src, inst.Name)
		dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Minute)
		_ = agentDelete(dctx, dst, inst.Name)
		dcancel()
		_ = s.db.SetInstanceStatus(ctx, inst.ID, "error", "迁移失败："+err.Error())
	}

	// 1. Reserve the destination's network before touching the source.
	var v4pool, v6pool string
	keepSrc := true
	if pl, perr := s.db.PlanByID(ctx, inst.PlanID); perr == nil {
		v4pool = pl.V4Pool
		v6pool = pl.V6Pool
		keepSrc = pl.KeepSourceIP
	}
	// Known gap: this claim only becomes durable when step 4 updates the
	// instance row, minutes later — a concurrent launch on dst could pick the
	// same address meanwhile. Acceptable for a rare admin action; the clash
	// surfaces as a failed provision, not silent corruption.
	var alloc *store.Allocation
	err := store.AllocSection(func() error {
		var aerr error
		alloc, aerr = s.db.Allocate(ctx, dst, store.AllocSpec{
			WantNAT: needsNAT(inst.Mode), WantDV4: needsDV4(inst.Mode),
			WantDV6: needsV6(inst.Mode), WantVNC: inst.InstanceType == "vm",
			V4Pool: v4pool, V6Pool: v6pool,
		})
		return aerr
	})
	if err != nil {
		fail("allocate", err)
		return
	}

	// 2. Stop source, snapshot to a portable backup.
	_ = agentAction(ctx, src, inst.Name, "stop", true)
	if err := agentBackupCreate(ctx, src, inst.Name); err != nil {
		fail("backup", err)
		return
	}

	// 3. Stream source → destination (imports with the same name + old config).
	if err := streamMigrate(ctx, src, dst, inst.Name); err != nil {
		fail("copy", err)
		return
	}

	// 4. Reconfigure the imported instance onto the destination's network and
	//    start it. Build the request from a copy carrying the new allocation.
	moved := *inst
	moved.NodeID = dst.ID
	moved.NATAddr = alloc.NATAddr
	moved.SSHPort = alloc.SSHPort
	moved.PortFrom = alloc.PortFrom
	moved.PortTo = alloc.PortTo
	moved.V6Addr = alloc.V6Addr
	moved.V4Addr = alloc.V4Addr
	if inst.InstanceType == "vm" {
		moved.VNCPort = alloc.VNCPort
	}
	req := buildCreateReq(dst, &moved, planFeatures(s, ctx, inst.PlanID), keepSrc, "")
	if err := agentReconfigure(ctx, dst, inst.Name, req); err != nil {
		fail("reconfigure", err)
		return
	}

	// 5. Success: commit the new location, then reclaim the source.
	_ = s.db.RelocateInstance(ctx, inst.ID, dst.ID, moved.NATAddr, moved.SSHPort,
		moved.PortFrom, moved.PortTo, moved.V6Addr, moved.V4Addr, moved.VNCPort)
	_ = s.db.SetInstanceStatus(ctx, inst.ID, "running", "")
	_ = agentBackupDelete(ctx, src, inst.Name)
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Minute)
	_ = agentDelete(dctx, src, inst.Name)
	dcancel()
	s.db.Audit(ctx, 0, "system", "instance.migrate.done",
		fmt.Sprintf("%s → %s", inst.Name, dst.Name), "")
}

// planFeatures loads a plan's feature list (empty if the plan is gone).
func planFeatures(s *Server, ctx context.Context, planID int64) []string {
	if planID <= 0 {
		return nil
	}
	if pl, err := s.db.PlanByID(ctx, planID); err == nil {
		return pl.FeatureList()
	}
	return nil
}

// ---------- ISO mount (VM, tenant) ----------

// handleAPIISOList returns the node's ISO library for the instance page.
func (s *Server) handleAPIISOList(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	if inst.InstanceType != "vm" {
		s.jsonErr(w, http.StatusBadRequest, "仅 KVM 虚拟机支持挂载 ISO")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	isos, err := agentISOs(ctx, node)
	if err != nil {
		s.jsonErr(w, http.StatusBadGateway, "读取 ISO 库失败："+err.Error())
		return
	}
	s.jsonOK(w, map[string]any{"isos": isos})
}

// handleAPIISOAttach mounts a library ISO on the VM.
func (s *Server) handleAPIISOAttach(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	if inst.InstanceType != "vm" {
		s.jsonErr(w, http.StatusBadRequest, "仅 KVM 虚拟机支持挂载 ISO")
		return
	}
	iso := formStr(r, "iso", 68)
	boot := formBool(r, "boot")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := agentISOAttach(ctx, node, inst.Name, iso, boot); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "挂载失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "iso.attach", inst.Name+" "+iso, clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true})
}

// handleAPIISODetach unmounts the VM's CD-ROM.
func (s *Server) handleAPIISODetach(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := agentISODetach(ctx, node, inst.Name); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "卸载失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "iso.detach", inst.Name, clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true})
}
