package panel

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"cubpanel/internal/store"
)

// handleAdminHome renders the overview dashboard.
func (s *Server) handleAdminHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "管理后台", "admin")

	nodes, _ := s.db.ListNodes(ctx, false)
	users, _ := s.db.CountUsers(ctx)
	insts, _ := s.db.CountInstances(ctx)
	total, unused, _ := s.db.CodeStats(ctx)
	audit, _ := s.db.ListAudit(ctx, 12)

	var cap, used, online int
	for _, n := range nodes {
		cap += n.MaxInstances
		used += n.InstanceCount
		if n.LastStatus == "ok" && time.Since(time.Unix(n.LastSeenAt, 0)) < 5*time.Minute {
			online++
		}
	}

	p.Data["Nodes"] = nodes
	p.Data["NodeOnline"] = online
	p.Data["Users"] = users
	p.Data["Instances"] = insts
	p.Data["Capacity"] = cap
	p.Data["Used"] = used
	p.Data["CodesTotal"] = total
	p.Data["CodesUnused"] = unused
	p.Data["Audit"] = audit
	s.render(w, r, "admin_home.html", p)
}

// ---------- nodes ----------

func (s *Server) handleAdminNodes(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "节点管理", "nodes")
	nodes, err := s.db.ListNodes(r.Context(), false)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取节点失败")
		return
	}
	p.Data["Nodes"] = nodes
	if r.URL.Query().Get("ok") != "" {
		p.Flash = "保存成功"
	}
	s.render(w, r, "admin_nodes.html", p)
}

// handleAdminNodeSave creates or updates a node definition.
func (s *Server) handleAdminNodeSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n := &store.Node{
		ID:           formInt64(r, "id"),
		Name:         formStr(r, "name", 32),
		Region:       formStr(r, "region", 32),
		Endpoint:     formStr(r, "endpoint", 200),
		Secret:       formStr(r, "secret", 200),
		CertFP:       formStr(r, "cert_fp", 120),
		StoragePool:  formStr(r, "storage_pool", 32),
		NATBridge:    formStr(r, "nat_bridge", 15),
		NATSubnet:    formStr(r, "nat_subnet", 43),
		NATManaged:   formBool(r, "nat_managed"),
		NATGW:        formStr(r, "nat_gw", 15),
		NATReserved:  formStr(r, "nat_reserved", 500),
		DNS:          formStr(r, "dns", 200),
		PortMin:      formInt(r, "port_min", 20000, 1024, 65535),
		PortMax:      formInt(r, "port_max", 60000, 1024, 65535),
		PortsEach:    formInt(r, "ports_each", 10, 0, 500),
		V6Enabled:    formBool(r, "v6_enabled"),
		V6Bridge:     formStr(r, "v6_bridge", 15),
		V6CIDR:       formStr(r, "v6_cidr", 60),
		V6GW:         formStr(r, "v6_gw", 45),
		V4Enabled:    formBool(r, "v4_enabled"),
		V4Bridge:     formStr(r, "v4_bridge", 15),
		V4CIDR:       formStr(r, "v4_cidr", 43),
		V4GW:         formStr(r, "v4_gw", 15),
		KVMEnabled:   formBool(r, "kvm_enabled"),
		MaxInstances: formInt(r, "max_instances", 50, 1, 100000),
		Enabled:      formBool(r, "enabled"),
	}

	if err := validateNode(n); err != nil {
		s.adminNodesError(w, r, err.Error())
		return
	}

	// An empty secret on an update means "keep the existing one", so operators
	// can edit a node without re-entering it.
	if n.ID > 0 && n.Secret == "" {
		if cur, err := s.db.NodeByID(ctx, n.ID); err == nil {
			n.Secret = cur.Secret
		}
	}
	if len(n.Secret) < 32 {
		s.adminNodesError(w, r, "节点密钥至少需要 32 位")
		return
	}
	if _, err := s.db.SaveNode(ctx, n); err != nil {
		s.adminNodesError(w, r, "保存失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "node.save", n.Name, clientIP(r))
	http.Redirect(w, r, "/admin/nodes?ok=1", http.StatusSeeOther)
}

func validateNode(n *store.Node) error {
	switch {
	case !slugRe.MatchString(n.Name):
		return fmt.Errorf("节点名只能使用小写字母、数字与连字符")
	case n.Endpoint == "" || (!strings.HasPrefix(n.Endpoint, "http://") && !strings.HasPrefix(n.Endpoint, "https://")):
		return fmt.Errorf("节点地址必须以 http:// 或 https:// 开头")
	case n.NATBridge == "":
		return fmt.Errorf("请填写 NAT 网桥名")
	case n.PortMax <= n.PortMin:
		return fmt.Errorf("端口范围上限必须大于下限")
	case n.V6Enabled && (n.V6Bridge == "" || n.V6CIDR == ""):
		return fmt.Errorf("启用独立 IPv6 时必须填写网桥与 IPv6 段")
	}
	// Reuse the allocator's parser so a bad CIDR is caught at save time rather
	// than at first redemption.
	if _, err := store.ValidateCIDR(n.NATSubnet, false); err != nil {
		return fmt.Errorf("NAT 子网无效：%v", err)
	}
	if err := store.ValidateReserved(n.NATReserved); err != nil {
		return fmt.Errorf("保留 IP 段无效：%v", err)
	}
	if n.NATGW != "" && !ipv4AddrRe.MatchString(n.NATGW) {
		return fmt.Errorf("NAT 网关格式不正确")
	}
	if n.CertFP != "" {
		fp := normalizeFP(n.CertFP)
		if len(fp) != 64 || !isHex(fp) {
			return fmt.Errorf("证书指纹应为 64 位十六进制（SHA-256）")
		}
		n.CertFP = fp
	}
	if n.DNS != "" {
		toks := strings.FieldsFunc(n.DNS, func(r rune) bool { return r == ' ' || r == ',' })
		if len(toks) > 8 {
			return fmt.Errorf("DNS 服务器最多 8 个")
		}
		for _, t := range toks {
			if net.ParseIP(t) == nil {
				return fmt.Errorf("DNS 服务器 %q 不是有效 IP", t)
			}
		}
	}
	if n.V6Enabled {
		if _, err := store.ValidateCIDR(n.V6CIDR, true); err != nil {
			return fmt.Errorf("IPv6 段无效：%v", err)
		}
	}
	if n.V4Enabled {
		if n.V4Bridge == "" || n.V4CIDR == "" {
			return fmt.Errorf("启用独立公网 IPv4 时必须填写网桥与 v4 段")
		}
		if _, err := store.ValidateCIDR(n.V4CIDR, false); err != nil {
			return fmt.Errorf("独立 IPv4 段无效：%v", err)
		}
		if n.V4GW != "" && !ipv4AddrRe.MatchString(n.V4GW) {
			return fmt.Errorf("独立 IPv4 网关格式不正确")
		}
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) adminNodesError(w http.ResponseWriter, r *http.Request, msg string) {
	p := s.newPage(r, "节点管理", "nodes")
	p.Error = msg
	nodes, _ := s.db.ListNodes(r.Context(), false)
	p.Data["Nodes"] = nodes
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "admin_nodes.html", p)
}

func (s *Server) handleAdminNodeDelete(w http.ResponseWriter, r *http.Request) {
	id := formInt64(r, "id")
	if err := s.db.DeleteNode(r.Context(), id); err != nil {
		s.adminNodesError(w, r, "删除失败：该节点仍有实例")
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "node.delete", fmt.Sprint(id), clientIP(r))
	http.Redirect(w, r, "/admin/nodes?ok=1", http.StatusSeeOther)
}

// handleAdminNodeProbe health-checks a node and records the result.
func (s *Server) handleAdminNodeProbe(w http.ResponseWriter, r *http.Request) {
	node, err := s.db.NodeByID(r.Context(), formInt64(r, "id"))
	if err != nil {
		s.jsonErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	info, err := agentHealth(ctx, node)
	if err != nil {
		_ = s.db.TouchNode(context.Background(), node.ID, "error: "+err.Error())
		s.jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = s.db.TouchNode(context.Background(), node.ID, "ok")
	s.jsonOK(w, info)
}

// ---------- plans ----------

func (s *Server) handleAdminPlans(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "套餐管理", "plans")
	plans, err := s.db.ListPlans(r.Context(), false)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取套餐失败")
		return
	}
	p.Data["Plans"] = plans
	p.Data["CachedImages"] = s.cachedImageAliases(r.Context())
	if r.URL.Query().Get("ok") != "" {
		p.Flash = "保存成功"
	}
	s.render(w, r, "admin_plans.html", p)
}

// cachedImageAliases returns the union of image aliases already cached
// (pre-warmed) across all enabled nodes. Plans offer these as checkboxes so
// only images that boot instantly can be sold.
// cachedImage is one pre-warmed alias with the variants available across the
// fleet, so the plan form can show whether a KVM plan's image is ready.
type cachedImage struct {
	Alias     string
	Container bool
	VM        bool
}

func (s *Server) cachedImageAliases(ctx context.Context) []cachedImage {
	nodes, _ := s.db.ListNodes(ctx, true)
	idx := map[string]*cachedImage{}
	for _, n := range nodes {
		c, cancel := context.WithTimeout(ctx, 8*time.Second)
		imgs, err := agentImages(c, n)
		cancel()
		if err != nil {
			continue // offline node: skip, don't block the page
		}
		for _, im := range imgs {
			for _, a := range im.Aliases {
				ci := idx[a]
				if ci == nil {
					ci = &cachedImage{Alias: a}
					idx[a] = ci
				}
				if im.Type == "virtual-machine" {
					ci.VM = true
				} else {
					ci.Container = true
				}
			}
		}
	}
	out := make([]cachedImage, 0, len(idx))
	for _, ci := range idx {
		out = append(out, *ci)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

func (s *Server) handleAdminPlanSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	pl := &store.Plan{
		ID:          formInt64(r, "id"),
		Name:        formStr(r, "name", 40),
		Description: formStr(r, "description", 200),
		CPU:         formInt(r, "cpu", 1, 1, 64),
		MemoryMB:    formInt(r, "memory_mb", 512, 64, 262144),
		DiskGB:      formInt(r, "disk_gb", 10, 1, 4096),
		Mode:        formStr(r, "mode", 8),
		// The form posts one value per checked image (or a comma list from
		// the free-text fallback); both collapse to a comma-joined string.
		Images:       strings.Join(r.Form["images"], ","),
		DurationDays: formInt(r, "duration_days", 30, 0, 3650),
		Enabled:      formBool(r, "enabled"),
		SortOrder:    formInt(r, "sort_order", 0, -999, 999),
	}
	if len(pl.Images) > 500 {
		s.adminPlansError(w, r, "镜像列表过长")
		return
	}
	if pl.Name == "" {
		s.adminPlansError(w, r, "请填写套餐名称")
		return
	}
	if !store.ValidPlanMode(pl.Mode) {
		pl.Mode = "nat"
	}
	pl.InstanceType = formStr(r, "instance_type", 12)
	if pl.InstanceType != "vm" {
		pl.InstanceType = "container"
	}
	var feats []string
	for _, f := range []string{"tun", "fuse", "privileged", "nesting"} {
		if formBool(r, "feat_"+f) {
			feats = append(feats, f)
		}
	}
	pl.Features = strings.Join(feats, ",")
	pl.TrafficGB = formInt(r, "traffic_gb", 0, 0, 1000000)
	pl.TrafficMode = trafficModeOr(formStr(r, "traffic_mode", 8))
	pl.RateDownMbps = formInt(r, "rate_down_mbps", 0, 0, 100000)
	pl.RateUpMbps = formInt(r, "rate_up_mbps", 0, 0, 100000)
	pl.ExtraBridges = formStr(r, "extra_bridges", 200)
	pl.V4Pool = formStr(r, "v4_pool", 200)
	pl.KeepSourceIP = formBool(r, "keep_source_ip")
	if pl.V4Pool != "" {
		if err := store.ValidateReserved(pl.V4Pool); err != nil {
			s.adminPlansError(w, r, "内网 IP 段格式不正确："+err.Error())
			return
		}
	}
	if v := strings.TrimSpace(r.FormValue("price")); v != "" {
		cents, err := parseMoney(v)
		if err != nil || cents < 0 {
			s.adminPlansError(w, r, "价格格式不正确（元，最多两位小数；0 或留空表示不可余额开通）")
			return
		}
		pl.PriceCents = cents
	}
	if err := validateImages(pl.Images); err != nil {
		s.adminPlansError(w, r, err.Error())
		return
	}
	if _, err := s.db.SavePlan(r.Context(), pl); err != nil {
		s.adminPlansError(w, r, "保存失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "plan.save", pl.Name, clientIP(r))
	http.Redirect(w, r, "/admin/plans?ok=1", http.StatusSeeOther)
}

// validateImages checks each alias against the format the agent accepts.
func validateImages(list string) error {
	items := strings.Split(list, ",")
	n := 0
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if !imageAliasRe.MatchString(it) {
			return fmt.Errorf("镜像别名 %q 格式不正确", it)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("至少需要配置一个系统镜像")
	}
	return nil
}

func (s *Server) adminPlansError(w http.ResponseWriter, r *http.Request, msg string) {
	p := s.newPage(r, "套餐管理", "plans")
	p.Error = msg
	plans, _ := s.db.ListPlans(r.Context(), false)
	p.Data["Plans"] = plans
	p.Data["CachedImages"] = s.cachedImageAliases(r.Context())
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "admin_plans.html", p)
}

func (s *Server) handleAdminPlanDelete(w http.ResponseWriter, r *http.Request) {
	id := formInt64(r, "id")
	if err := s.db.DeletePlan(r.Context(), id); err != nil {
		s.adminPlansError(w, r, "删除失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "plan.delete", fmt.Sprint(id), clientIP(r))
	http.Redirect(w, r, "/admin/plans?ok=1", http.StatusSeeOther)
}

// ---------- images ----------

// handleAdminImages shows one node's cached images alongside what the image
// server currently offers, so operators can verify plan aliases and pre-warm
// downloads.
func (s *Server) handleAdminImages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "镜像管理", "images")

	nodes, err := s.db.ListNodes(ctx, false)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取节点失败")
		return
	}
	p.Data["Nodes"] = nodes
	p.Data["Cached"] = map[string]bool{}
	p.Data["CachedVM"] = map[string]bool{}

	var node *store.Node
	if id := formInt64(r, "node"); id > 0 {
		for _, n := range nodes {
			if n.ID == id {
				node = n
				break
			}
		}
	} else if len(nodes) > 0 {
		node = nodes[0]
	}

	if node != nil {
		p.Data["Node"] = node
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if local, err := agentImages(cctx, node); err == nil {
			p.Data["Local"] = local
			cached := make(map[string]bool, len(local))
			cachedVM := map[string]bool{}
			for _, im := range local {
				for _, a := range im.Aliases {
					if im.Type == "virtual-machine" {
						cachedVM[a] = true
					} else {
						cached[a] = true
					}
				}
			}
			p.Data["Cached"] = cached
			p.Data["CachedVM"] = cachedVM
		} else {
			p.Error = "读取节点镜像失败：" + err.Error()
		}
		if remote, err := agentRemoteImages(cctx, node); err == nil {
			p.Data["Remote"] = remote
		} else if p.Error == "" {
			p.Error = "读取镜像源目录失败：" + err.Error()
		}
		if isos, err := agentISOs(cctx, node); err == nil {
			p.Data["ISOs"] = isos
		}
	}

	switch r.URL.Query().Get("ok") {
	case "pull":
		p.Flash = "已开始拉取镜像。下载在后台进行，完成情况见操作日志，稍后刷新本页即可看到。"
	case "del":
		p.Flash = "镜像已删除"
	case "iso":
		p.Flash = "已开始下载 ISO。完成情况见操作日志，稍后刷新本页即可看到。"
	case "isodel":
		p.Flash = "ISO 已删除"
	}
	s.render(w, r, "admin_images.html", p)
}

// handleAdminImagePull pre-warms one image onto a node in the background.
func (s *Server) handleAdminImagePull(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	node, err := s.db.NodeByID(ctx, formInt64(r, "node"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "节点不存在")
		return
	}
	alias := formStr(r, "alias", 64)
	if !imageAliasRe.MatchString(alias) {
		s.renderError(w, r, http.StatusBadRequest, "镜像别名格式不正确")
		return
	}
	imageType := "container"
	if formStr(r, "type", 20) == "virtual-machine" {
		imageType = "virtual-machine"
	}
	label := alias
	if imageType == "virtual-machine" {
		label = alias + " (VM)"
	}

	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "image.pull", node.Name+" "+label, clientIP(r))

	// Image downloads take minutes; run them detached and report through the
	// audit log rather than holding the admin's request open.
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := agentImagePull(bctx, node, alias, imageType); err != nil {
			s.db.Audit(context.Background(), 0, "system", "image.pull.fail", node.Name+" "+label+": "+err.Error(), "")
			return
		}
		s.db.Audit(context.Background(), 0, "system", "image.pull.done", node.Name+" "+label, "")
	}()

	http.Redirect(w, r, "/admin/images?node="+fmt.Sprint(node.ID)+"&ok=pull", http.StatusSeeOther)
}

// handleAdminImageDelete drops one cached image from a node.
func (s *Server) handleAdminImageDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	node, err := s.db.NodeByID(ctx, formInt64(r, "node"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "节点不存在")
		return
	}
	fp := formStr(r, "fingerprint", 64)
	if !imageFpRe.MatchString(fp) {
		s.renderError(w, r, http.StatusBadRequest, "镜像指纹格式不正确")
		return
	}

	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := agentImageDelete(dctx, node, fp); err != nil {
		s.renderError(w, r, http.StatusBadGateway, "删除失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "image.delete", node.Name+" "+fp[:12], clientIP(r))
	http.Redirect(w, r, "/admin/images?node="+fmt.Sprint(node.ID)+"&ok=del", http.StatusSeeOther)
}

// ---------- activation codes ----------

func (s *Server) handleAdminCodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "激活码", "codes")

	batch := formStr(r, "batch", 32)
	onlyUnused := r.URL.Query().Get("unused") == "1"

	codes, err := s.db.ListCodes(ctx, batch, onlyUnused, 300)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取激活码失败")
		return
	}
	plans, _ := s.db.ListPlans(ctx, false)
	nodes, _ := s.db.ListNodes(ctx, false)
	total, unused, _ := s.db.CodeStats(ctx)

	p.Data["Codes"] = codes
	p.Data["Plans"] = plans
	p.Data["Nodes"] = nodes
	p.Data["Batch"] = batch
	p.Data["OnlyUnused"] = onlyUnused
	p.Data["Total"] = total
	p.Data["Unused"] = unused
	s.render(w, r, "admin_codes.html", p)
}

// handleAdminCodeGen mints a batch of activation codes.
func (s *Server) handleAdminCodeGen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	planID := formInt64(r, "plan_id")
	nodeID := formInt64(r, "node_id")
	count := formInt(r, "count", 10, 1, 500)
	note := formStr(r, "note", 100)
	validDays := formInt(r, "valid_days", 0, 0, 3650)

	fail := func(msg string) {
		p := s.newPage(r, "激活码", "codes")
		p.Error = msg
		codes, _ := s.db.ListCodes(ctx, "", false, 300)
		plans, _ := s.db.ListPlans(ctx, false)
		nodes, _ := s.db.ListNodes(ctx, false)
		total, unused, _ := s.db.CodeStats(ctx)
		p.Data["Codes"], p.Data["Plans"], p.Data["Nodes"] = codes, plans, nodes
		p.Data["Total"], p.Data["Unused"] = total, unused
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "admin_codes.html", p)
	}

	plan, err := s.db.PlanByID(ctx, planID)
	if err != nil {
		fail("请选择有效的套餐")
		return
	}
	if nodeID > 0 {
		if _, err := s.db.NodeByID(ctx, nodeID); err != nil {
			fail("请选择有效的节点")
			return
		}
	}

	batch := time.Now().Format("20060102-150405")
	codes := make([]string, count)
	for i := range codes {
		codes[i] = newActivationCode()
	}
	var expiresAt int64
	if validDays > 0 {
		expiresAt = time.Now().AddDate(0, 0, validDays).Unix()
	}
	if err := s.db.InsertCodes(ctx, codes, planID, nodeID, batch, note, expiresAt); err != nil {
		fail("生成失败：" + err.Error())
		return
	}

	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "code.generate",
		fmt.Sprintf("%d 个 / %s / batch %s", count, plan.Name, batch), clientIP(r))

	// Show the freshly minted batch so the operator can copy it out.
	p := s.newPage(r, "激活码", "codes")
	p.Flash = fmt.Sprintf("已生成 %d 个激活码（批次 %s）", count, batch)
	p.Data["New"] = codes
	all, _ := s.db.ListCodes(ctx, "", false, 300)
	plans, _ := s.db.ListPlans(ctx, false)
	nodes, _ := s.db.ListNodes(ctx, false)
	total, unused, _ := s.db.CodeStats(ctx)
	p.Data["Codes"], p.Data["Plans"], p.Data["Nodes"] = all, plans, nodes
	p.Data["Total"], p.Data["Unused"] = total, unused
	s.render(w, r, "admin_codes.html", p)
}

func (s *Server) handleAdminCodeDelete(w http.ResponseWriter, r *http.Request) {
	id := formInt64(r, "id")
	_ = s.db.DeleteCode(r.Context(), id)
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "code.delete", fmt.Sprint(id), clientIP(r))
	http.Redirect(w, r, "/admin/codes", http.StatusSeeOther)
}

// newActivationCode returns a 16-character grouped code drawn from a
// human-friendly alphabet. That is ~82 bits of entropy, far beyond guessing.
func newActivationCode() string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	out := make([]byte, 0, 19)
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, alpha[int(v)%len(alpha)])
	}
	return string(out)
}

// ---------- users ----------

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "用户管理", "users")
	users, err := s.db.ListUsers(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取用户失败")
		return
	}
	p.Data["Users"] = users
	p.Data["APIKey"] = s.db.Setting(r.Context(), "recharge_api_key", "")
	if r.URL.Query().Get("ok") == "key" {
		p.Flash = "充值 API 密钥已生成"
	}
	s.render(w, r, "admin_users.html", p)
}

// handleAdminUserAction suspends, restores, promotes or resets an account.
func (s *Server) handleAdminUserAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	id := formInt64(r, "id")
	action := formStr(r, "action", 16)

	target, err := s.db.UserByID(ctx, id)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "用户不存在")
		return
	}
	// An administrator must not be able to lock themselves out.
	if target.ID == ac.User.ID && action != "reset" {
		s.renderError(w, r, http.StatusBadRequest, "不能对自己执行该操作")
		return
	}

	switch action {
	case "suspend":
		_ = s.db.SetUserStatus(ctx, id, "suspended")
		_ = s.db.DeleteUserSessions(ctx, id)
	case "activate":
		_ = s.db.SetUserStatus(ctx, id, "active")
	case "promote":
		_ = s.db.SetUserAdmin(ctx, id, true)
	case "demote":
		_ = s.db.SetUserAdmin(ctx, id, false)
		// Drop their sessions so cached admin UI stops working immediately.
		_ = s.db.DeleteUserSessions(ctx, id)
	case "reset":
		pw := randomPassword(14)
		hash, err := hashPassword(pw)
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "重置失败")
			return
		}
		_ = s.db.SetUserPassword(ctx, id, hash)
		_ = s.db.DeleteUserSessions(ctx, id)
		s.db.Audit(ctx, ac.User.ID, ac.User.Email, "user.reset", target.Email, clientIP(r))

		p := s.newPage(r, "用户管理", "users")
		p.Flash = fmt.Sprintf("已重置 %s 的密码", target.Email)
		p.Data["NewPassword"] = pw
		users, _ := s.db.ListUsers(ctx)
		p.Data["Users"] = users
		s.render(w, r, "admin_users.html", p)
		return
	default:
		s.renderError(w, r, http.StatusBadRequest, "不支持的操作")
		return
	}

	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "user."+action, target.Email, clientIP(r))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// ---------- instances ----------

func (s *Server) handleAdminInstances(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "实例管理", "instances")
	list, err := s.db.ListInstances(r.Context(), 0)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取实例失败")
		return
	}
	p.Data["Instances"] = list
	nodes, _ := s.db.ListNodes(r.Context(), false)
	p.Data["Nodes"] = nodes
	s.render(w, r, "admin_instances.html", p)
}

// handleAdminInstanceDelete destroys a container and its record.
func (s *Server) handleAdminInstanceDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := formInt64(r, "id")
	inst, err := s.db.InstanceByID(ctx, id)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	if node, err := s.db.NodeByID(ctx, inst.NodeID); err == nil {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		// A node-side failure must not strand the panel record; log and go on.
		if err := agentDelete(dctx, node, inst.Name); err != nil {
			s.db.Audit(ctx, 0, "system", "instance.delete.warn",
				inst.Name+": "+err.Error(), clientIP(r))
		}
		cancel()
	}
	_ = s.db.DeleteInstance(ctx, id)

	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.delete", inst.Name, clientIP(r))
	http.Redirect(w, r, "/admin/instances", http.StatusSeeOther)
}

// handleAdminInstanceResize applies new limits to a live instance.
func (s *Server) handleAdminInstanceResize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inst, err := s.db.InstanceByID(ctx, formInt64(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	node, err := s.db.NodeByID(ctx, inst.NodeID)
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "节点不可用")
		return
	}
	cpu := formInt(r, "cpu", inst.CPU, 1, 64)
	mem := formInt(r, "memory_mb", inst.MemoryMB, 64, 262144)
	disk := formInt(r, "disk_gb", inst.DiskGB, 1, 4096)
	rateDown := formInt(r, "rate_down_mbps", inst.RateDownMbps, 0, 100000)
	rateUp := formInt(r, "rate_up_mbps", inst.RateUpMbps, 0, 100000)
	if disk < inst.DiskGB {
		s.renderError(w, r, http.StatusBadRequest, "磁盘只能扩大，不能缩小")
		return
	}

	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := agentResize(dctx, node, inst.Name, cpu, mem, disk, rateDown, rateUp); err != nil {
		s.renderError(w, r, http.StatusBadGateway, "调整失败："+err.Error())
		return
	}
	_ = s.db.ResizeInstance(ctx, inst.ID, cpu, mem, disk, rateDown, rateUp, 0)

	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.resize",
		fmt.Sprintf("%s → %dC/%dM/%dG %d/%dMbps", inst.Name, cpu, mem, disk, rateDown, rateUp), clientIP(r))
	http.Redirect(w, r, "/admin/instances", http.StatusSeeOther)
}

// handleAdminInstanceTrafficReset zeroes an instance's metered usage and
// lifts an overquota stop.
func (s *Server) handleAdminInstanceTrafficReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inst, err := s.db.InstanceByID(ctx, formInt64(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	next := int64(0)
	if inst.TrafficLimitGB > 0 {
		next = time.Now().AddDate(0, 0, 30).Unix()
	}
	_ = s.db.ResetTraffic(ctx, inst.ID, next)
	if inst.Status == "overquota" {
		_ = s.db.SetInstanceStatus(ctx, inst.ID, "stopped", "")
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.traffic.reset", inst.Name, clientIP(r))
	http.Redirect(w, r, "/admin/instances", http.StatusSeeOther)
}

// handleAdminInstanceExtend pushes an instance's expiry out.
func (s *Server) handleAdminInstanceExtend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := formInt64(r, "id")
	days := formInt(r, "days", 30, 1, 3650)

	inst, err := s.db.InstanceByID(ctx, id)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	if err := s.db.ExtendInstance(ctx, id, days); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "续期失败")
		return
	}
	// A renewed instance is no longer expired.
	if inst.Status == "expired" {
		_ = s.db.SetInstanceStatus(ctx, id, "stopped", "")
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.extend",
		fmt.Sprintf("%s +%d 天", inst.Name, days), clientIP(r))
	http.Redirect(w, r, "/admin/instances", http.StatusSeeOther)
}

// ---------- audit ----------

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "操作日志", "audit")
	entries, err := s.db.ListAudit(r.Context(), 300)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取日志失败")
		return
	}
	p.Data["Audit"] = entries
	s.render(w, r, "admin_audit.html", p)
}

// ---------- site settings ----------

// handleAdminSettings renders the site + payment configuration form.
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "网站设置", "settings")
	get := func(k, def string) string { return s.db.Setting(ctx, k, def) }
	p.Data["SiteName"] = get("site_name", s.cfg.SiteName)
	p.Data["Announcement"] = get("announcement", "")
	p.Data["HideRepoLink"] = get("hide_repo_link", "") == "1"
	p.Data["RepoURL"] = repoURL
	p.Data["EpayURL"] = get("pay_epay_url", "")
	p.Data["EpayPID"] = get("pay_epay_pid", "")
	p.Data["EpayKey"] = get("pay_epay_key", "")
	p.Data["Alipay"] = get("pay_alipay", "") == "1"
	p.Data["Wxpay"] = get("pay_wxpay", "") == "1"
	p.Data["USDTOn"] = get("pay_usdt", "") == "1"
	p.Data["USDTAddr"] = get("pay_usdt_addr", "")
	p.Data["USDTNet"] = get("pay_usdt_net", "TRC20")
	p.Data["USDTRate"] = get("pay_usdt_rate", "")
	if r.URL.Query().Get("ok") != "" {
		p.Flash = "已保存"
	}
	s.render(w, r, "admin_settings.html", p)
}

// handleAdminSettingsSave persists the site + payment configuration.
func (s *Server) handleAdminSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set := func(k, v string) { _ = s.db.SetSetting(ctx, k, v) }
	set("site_name", formStr(r, "site_name", 40))
	set("announcement", formStr(r, "announcement", 2000))
	set("hide_repo_link", boolStr(formBool(r, "hide_repo_link")))
	set("pay_epay_url", formStr(r, "pay_epay_url", 200))
	set("pay_epay_pid", formStr(r, "pay_epay_pid", 64))
	set("pay_epay_key", formStr(r, "pay_epay_key", 128))
	set("pay_alipay", boolStr(formBool(r, "pay_alipay")))
	set("pay_wxpay", boolStr(formBool(r, "pay_wxpay")))
	set("pay_usdt", boolStr(formBool(r, "pay_usdt")))
	set("pay_usdt_addr", formStr(r, "pay_usdt_addr", 128))
	set("pay_usdt_net", formStr(r, "pay_usdt_net", 16))
	if rate := strings.TrimSpace(r.FormValue("pay_usdt_rate")); rate != "" {
		if _, err := parseMoney(rate); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "USDT 汇率格式不正确（元，最多两位小数）")
			return
		}
		set("pay_usdt_rate", rate)
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "settings.save", "site & payment", clientIP(r))
	http.Redirect(w, r, "/admin/settings?ok=1", http.StatusSeeOther)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---------- recharge orders ----------

func (s *Server) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "充值订单", "orders")
	orders, err := s.db.ListOrders(r.Context(), false, 300)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取订单失败")
		return
	}
	p.Data["Orders"] = orders
	if r.URL.Query().Get("ok") != "" {
		p.Flash = "已确认到账"
	}
	s.render(w, r, "admin_orders.html", p)
}

// handleAdminOrderConfirm manually marks an order paid (USDT / offline) and
// credits the user.
func (s *Server) handleAdminOrderConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := s.db.OrderByID(ctx, formInt64(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "订单不存在")
		return
	}
	if _, err := s.db.MarkOrderPaid(ctx, o.OrderNo, o.TxID); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "确认失败")
		return
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "recharge.confirm", o.OrderNo, clientIP(r))
	http.Redirect(w, r, "/admin/orders?ok=1", http.StatusSeeOther)
}

// ---------- ISO library ----------

var isoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.iso$`)

// handleAdminISOPull downloads an ISO into a node's library from a URL.
func (s *Server) handleAdminISOPull(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	node, err := s.db.NodeByID(ctx, formInt64(r, "node"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "节点不存在")
		return
	}
	url := formStr(r, "url", 500)
	name := formStr(r, "name", 68)
	if name == "" {
		if i := strings.LastIndex(url, "/"); i >= 0 && i+1 < len(url) {
			name = url[i+1:]
		}
		if q := strings.IndexAny(name, "?#"); q >= 0 {
			name = name[:q]
		}
	}
	if !strings.HasSuffix(strings.ToLower(name), ".iso") {
		name += ".iso"
	}
	if !isoNameRe.MatchString(name) {
		s.renderError(w, r, http.StatusBadRequest, "ISO 文件名不合法（只能字母数字与 . _ -，且以 .iso 结尾）")
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		s.renderError(w, r, http.StatusBadRequest, "URL 必须以 http:// 或 https:// 开头")
		return
	}

	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "iso.pull", node.Name+" "+name, clientIP(r))
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()
		if err := agentISOPull(bctx, node, url, name); err != nil {
			s.db.Audit(context.Background(), 0, "system", "iso.pull.fail", node.Name+" "+name+": "+err.Error(), "")
			return
		}
		s.db.Audit(context.Background(), 0, "system", "iso.pull.done", node.Name+" "+name, "")
	}()
	http.Redirect(w, r, "/admin/images?node="+fmt.Sprint(node.ID)+"&ok=iso", http.StatusSeeOther)
}

// handleAdminISODelete removes an ISO from a node's library.
func (s *Server) handleAdminISODelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	node, err := s.db.NodeByID(ctx, formInt64(r, "node"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "节点不存在")
		return
	}
	name := formStr(r, "name", 68)
	if !isoNameRe.MatchString(name) {
		s.renderError(w, r, http.StatusBadRequest, "ISO 文件名不合法")
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := agentISODelete(dctx, node, name); err != nil {
		s.renderError(w, r, http.StatusBadGateway, "删除失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "iso.delete", node.Name+" "+name, clientIP(r))
	http.Redirect(w, r, "/admin/images?node="+fmt.Sprint(node.ID)+"&ok=isodel", http.StatusSeeOther)
}
