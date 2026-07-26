// Storage-pool reporting and management for the panel's storage page.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"cubpanel/internal/shared"
)

// hasKernelFS reports whether a filesystem name appears in /proc/filesystems.
func hasKernelFS(name string) bool {
	b, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(strings.TrimPrefix(line, "nodev")) == name {
			return true
		}
	}
	return false
}

// hasKernelModule reports whether a module is loaded (or builtin via sysfs).
func hasKernelModule(name string) bool {
	_, err := os.Stat("/sys/module/" + name)
	return err == nil
}

// handleStorage answers GET /v1/storage with driver support + pool usage.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request, _ []byte) {
	ctx := r.Context()
	rep := shared.StorageReport{
		// dm-thin may be builtin, a loaded module, or only visible once lvm is
		// active; any of the three counts as "the host can do lvm-thin".
		LVM:   hasKernelModule("dm_thin_pool") || hasKernelModule("dm_mod"),
		Btrfs: hasKernelFS("btrfs") || hasKernelModule("btrfs"),
		ZFS:   hasKernelFS("zfs") || hasKernelModule("zfs"),
	}
	pools, err := s.lxd.StoragePools(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	for _, p := range pools {
		info := shared.StoragePoolInfo{Name: p.Name, Driver: p.Driver}
		for _, u := range p.UsedBy {
			if strings.HasPrefix(u, "/1.0/instances/") {
				info.Instances++
			}
		}
		if used, total, err := s.lxd.StoragePoolUsage(ctx, p.Name); err == nil {
			info.UsedBytes, info.TotalBytes = used, total
		}
		rep.Pools = append(rep.Pools, info)
	}
	writeJSON(w, http.StatusOK, rep)
}

var poolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// handleStorageResize grows a pool. Incus rejects shrinking, so the only
// validation needed here is a sane bound.
func (s *Server) handleStorageResize(w http.ResponseWriter, r *http.Request, body []byte) {
	pool := r.PathValue("pool")
	if !poolNameRe.MatchString(pool) {
		writeErr(w, http.StatusBadRequest, "invalid pool name")
		return
	}
	var req shared.StorageResizeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.SizeGiB < 5 || req.SizeGiB > 1<<20 {
		writeErr(w, http.StatusBadRequest, "size out of range (5GiB – 1PiB)")
		return
	}
	if err := s.lxd.StoragePoolSetSize(r.Context(), pool, fmt.Sprintf("%dGiB", req.SizeGiB)); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resized"})
}

// extraDiskVolume names the pool volume backing data disk #i of an instance.
func extraDiskVolume(inst string, i int) string { return inst + "-d" + fmt.Sprint(i) }

// createExtraDisks provisions the plan's data volumes before instance
// creation; on any failure it removes what it already made.
func (s *Server) createExtraDisks(ctx context.Context, pool, inst string, sizes []int) error {
	for i, gb := range sizes {
		if gb < 1 || gb > 4096 || i >= 4 {
			return fmt.Errorf("extra disk %d out of range", i)
		}
		if err := s.lxd.VolumeCreate(ctx, pool, extraDiskVolume(inst, i), gb); err != nil {
			for j := 0; j < i; j++ {
				_ = s.lxd.VolumeDelete(ctx, pool, extraDiskVolume(inst, j))
			}
			return fmt.Errorf("create data volume %d: %w", i, err)
		}
	}
	return nil
}

// deleteExtraDisks removes any data volumes an instance may own (best effort;
// slots beyond what existed simply do not match anything).
func (s *Server) deleteExtraDisks(ctx context.Context, pool, inst string) {
	for i := 0; i < 4; i++ {
		_ = s.lxd.VolumeDelete(ctx, pool, extraDiskVolume(inst, i))
	}
}
