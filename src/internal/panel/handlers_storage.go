// Admin storage page: per-node pool usage, driver support and pool growing.
package panel

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"cubpanel/internal/shared"
	"cubpanel/internal/store"
)

// nodeStorage is one node's report (or the error reaching it) for the page.
type nodeStorage struct {
	Node   *store.Node
	Report *shared.StorageReport
	Err    string
}

// PoolRow adds display helpers over the wire type.
type poolRow struct {
	shared.StoragePoolInfo
	UsedGiB  string
	TotalGiB string
	Percent  int
}

func gib(b int64) string {
	return fmt.Sprintf("%.1f", float64(b)/(1<<30))
}

// handleAdminStorage renders the storage overview across every node.
func (s *Server) handleAdminStorage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "存储管理", "storage")

	nodes, err := s.db.ListNodes(ctx, false)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取节点失败")
		return
	}

	// Probe every node concurrently; a slow/unreachable one only greys out
	// its own card, never the whole page.
	rows := make([]*nodeStorage, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n *store.Node) {
			defer wg.Done()
			row := &nodeStorage{Node: n}
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			rep, err := agentStorage(cctx, n)
			if err != nil {
				row.Err = err.Error()
			} else {
				row.Report = rep
			}
			rows[i] = row
		}(i, n)
	}
	wg.Wait()

	// Flatten into template-friendly rows with usage percentages.
	type nodeView struct {
		Node  *store.Node
		Err   string
		LVM   bool
		Btrfs bool
		ZFS   bool
		Pools []poolRow
	}
	var views []nodeView
	for _, row := range rows {
		v := nodeView{Node: row.Node, Err: row.Err}
		if row.Report != nil {
			v.LVM, v.Btrfs, v.ZFS = row.Report.LVM, row.Report.Btrfs, row.Report.ZFS
			for _, pi := range row.Report.Pools {
				pr := poolRow{StoragePoolInfo: pi, UsedGiB: gib(pi.UsedBytes), TotalGiB: gib(pi.TotalBytes)}
				if pi.TotalBytes > 0 {
					pr.Percent = int(pi.UsedBytes * 100 / pi.TotalBytes)
				}
				v.Pools = append(v.Pools, pr)
			}
		}
		views = append(views, v)
	}
	p.Data["Nodes"] = views
	if r.URL.Query().Get("ok") != "" {
		p.Flash = "存储池已扩容"
	}
	s.render(w, r, "admin_storage.html", p)
}

var poolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// handleAdminStorageResize grows a pool on one node (shrinking is refused by
// Incus, so this is grow-only by construction).
func (s *Server) handleAdminStorageResize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	node, err := s.db.NodeByID(ctx, formInt64(r, "node"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "节点不存在")
		return
	}
	pool := formStr(r, "pool", 64)
	size := formInt(r, "size_gib", 0, 5, 1<<20)
	if !poolNameRe.MatchString(pool) || size < 5 {
		s.renderError(w, r, http.StatusBadRequest, "参数无效")
		return
	}
	if err := agentStorageResize(ctx, node, pool, size); err != nil {
		s.renderError(w, r, http.StatusBadGateway, "扩容失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "storage.resize",
		fmt.Sprintf("%s/%s -> %dGiB", node.Name, pool, size), clientIP(r))
	http.Redirect(w, r, "/admin/storage?ok=1", http.StatusSeeOther)
}
