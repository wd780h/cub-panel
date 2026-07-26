// Snapshot and cross-node migration endpoints. Snapshots wrap the Incus
// snapshot API; migration streams an Incus backup tarball (the panel is the
// trusted middle that pipes source→destination, so per-node secrets never
// leave the master).
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"cubpanel/internal/lxd"
	"cubpanel/internal/shared"
)

var snapRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,31}$`)

// migrateBackup is the fixed backup name used during a migration.
const migrateBackup = "cub-panel-migrate"

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	snaps, err := s.lxd.Snapshots(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]shared.SnapshotInfo, 0, len(snaps))
	for _, sn := range snaps {
		si := shared.SnapshotInfo{Name: sn.Name, SizeBytes: sn.Size}
		if t, err := time.Parse(time.RFC3339, sn.CreatedAt); err == nil {
			si.CreatedAt = t.Unix()
		}
		out = append(out, si)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req shared.SnapshotRequest
	if err := json.Unmarshal(body, &req); err != nil || !snapRe.MatchString(req.Snapshot) {
		writeErr(w, http.StatusBadRequest, "invalid snapshot name")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.lxd.SnapshotCreate(ctx, name, req.Snapshot); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "created"})
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	snap := r.PathValue("snap")
	if !snapRe.MatchString(snap) {
		writeErr(w, http.StatusBadRequest, "invalid snapshot name")
		return
	}
	if err := s.lxd.SnapshotDelete(r.Context(), name, snap); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req shared.SnapshotRequest
	if err := json.Unmarshal(body, &req); err != nil || !snapRe.MatchString(req.Snapshot) {
		writeErr(w, http.StatusBadRequest, "invalid snapshot name")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.lxd.SnapshotRestore(ctx, name, req.Snapshot); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

// ---------- migration ----------

// handleBackupCreate makes a portable backup tarball ready to export.
func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	_ = s.lxd.BackupDelete(ctx, name, migrateBackup) // clear any stale one
	if err := s.lxd.BackupCreate(ctx, name, migrateBackup); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleBackupExport streams the migration backup tarball. Signed like every
// other call; the body is the raw tarball, not JSON.
func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	if err := s.lxd.BackupExport(ctx, name, migrateBackup, w); err != nil {
		s.log("backup export %s: %v", name, err)
	}
}

// handleBackupDelete removes the migration backup after a successful copy.
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request, _ []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	_ = s.lxd.BackupDelete(r.Context(), name, migrateBackup)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleImport re-creates an instance from a streamed backup tarball, then
// PATCHes its devices to this node's network before starting it.
func (s *Server) handleImportStream(w http.ResponseWriter, r *http.Request) {
	// The body is a large raw tarball we cannot buffer to hash, so — like the
	// console upgrade — the signature covers an empty body over TLS.
	if err := shared.Verify(s.cfg.Secret, r, nil); err != nil {
		s.log("import auth reject: %v", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.nonces.use(r.Header.Get(shared.HeaderNonce)) {
		writeErr(w, http.StatusUnauthorized, "replayed request")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	if err := s.lxd.BackupImport(ctx, r.Body); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "imported"})
}

// handleReconfigure applies a fresh device/config set to an instance (used
// after import to move it onto the destination node's network) and starts it.
func (s *Server) handleReconfigure(w http.ResponseWriter, r *http.Request, body []byte) {
	name, ok := instName(w, r)
	if !ok {
		return
	}
	var req shared.CreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	req.Name = name
	put := lxd.InstancePut{Devices: s.buildDevices(&req)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.lxd.Update(ctx, name, put); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.useAgentDNAT(&req) {
		_ = s.applyDNAT(ctx, name, dnatSpec{Addr: req.NATAddr, SSHPort: req.SSHPort, PortFrom: req.PortFrom, PortTo: req.PortTo})
	}
	if err := s.lxd.SetState(ctx, name, "start", false); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reconfigured"})
}
