package panel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"cubpanel/internal/shared"
	"cubpanel/internal/store"
)

// maxInstancesPerUser caps how many containers one account may hold.
const maxInstancesPerUser = 20

// handleDashboard lists the caller's instances.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "控制台", "dashboard")

	list, err := s.db.ListInstances(ctx, p.User.ID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取实例失败")
		return
	}
	p.Data["Instances"] = list

	var running int
	for _, i := range list {
		if i.Status == "running" {
			running++
		}
	}
	p.Data["Running"] = running
	p.Data["Total"] = len(list)
	s.render(w, r, "dashboard.html", p)
}

// handleRedeemPage renders the activation-code form.
func (s *Server) handleRedeemPage(w http.ResponseWriter, r *http.Request) {
	// Activation-code redemption now lives on the merged 创建实例 page; keep
	// the old path working by forwarding, preserving any prefilled code.
	target := "/app/deploy"
	if c := formStr(r, "code", 64); c != "" {
		target += "?code=" + urlQueryEscape(c)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ownedInstance loads an instance and enforces ownership (admins bypass).
func (s *Server) ownedInstance(w http.ResponseWriter, r *http.Request) (*store.Instance, *store.Node, bool) {
	id, ok := pathID(r)
	if !ok {
		s.jsonErr(w, http.StatusBadRequest, "参数无效")
		return nil, nil, false
	}
	ac := userFrom(r)
	inst, err := s.db.InstanceByID(r.Context(), id)
	if err != nil {
		// Do not distinguish "missing" from "not yours".
		s.jsonErr(w, http.StatusNotFound, "实例不存在")
		return nil, nil, false
	}
	if inst.UserID != ac.User.ID && !ac.User.IsAdmin {
		s.jsonErr(w, http.StatusNotFound, "实例不存在")
		return nil, nil, false
	}
	node, err := s.db.NodeByID(r.Context(), inst.NodeID)
	if err != nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "节点不可用")
		return nil, nil, false
	}
	return inst, node, true
}

// handleInstancePage renders the instance detail view.
func (s *Server) handleInstancePage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "参数无效")
		return
	}
	ac := userFrom(r)
	inst, err := s.db.InstanceByID(r.Context(), id)
	if err != nil || (inst.UserID != ac.User.ID && !ac.User.IsAdmin) {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	node, _ := s.db.NodeByID(r.Context(), inst.NodeID)

	p := s.newPage(r, inst.Name, "dashboard")
	p.Data["Inst"] = inst
	// Only the connection host is exposed, never the agent endpoint or secret.
	if node != nil {
		p.Data["SSHHost"] = hostOnly(node.Endpoint)
	}
	p.Data["Upgrades"] = s.upgradeCandidates(r.Context(), inst)
	p.Data["Balance"] = ac.User.BalanceCents
	p.Data["OldPrice"] = s.instancePlanPrice(r.Context(), inst)
	switch {
	case r.URL.Query().Get("ok") == "upgrade":
		p.Flash = "升级成功，新配置已生效。"
	case r.URL.Query().Get("err") != "":
		p.Error = map[string]string{
			"upgrade_state":        "实例当前状态不允许升级",
			"upgrade_bad":          "所选套餐不满足升级条件（需同网络模式、规格不小于当前、价格更高）",
			"upgrade_insufficient": "余额不足，请先充值",
			"upgrade_fail":         "升级失败，请稍后再试或联系管理员",
		}[r.URL.Query().Get("err")]
	}
	s.render(w, r, "instance.html", p)
}

// handleConsolePage renders the terminal shell page.
func (s *Server) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "参数无效")
		return
	}
	ac := userFrom(r)
	inst, err := s.db.InstanceByID(r.Context(), id)
	if err != nil || (inst.UserID != ac.User.ID && !ac.User.IsAdmin) {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	p := s.newPage(r, "终端 · "+inst.Name, "dashboard")
	p.Data["Inst"] = inst
	s.render(w, r, "console.html", p)
}

// ---------- redeem ----------

// handleRedeemPost validates an activation code and provisions an instance.
func (s *Server) handleRedeemPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	raw := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	label := formStr(r, "label", 32)
	image := formStr(r, "image", 64)

	fail := func(msg string) {
		p := s.deployPage(r)
		p.Error = msg
		p.Data["Code"] = raw // reopens the activation-code panel with the value
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "deploy.html", p)
	}

	if !codeRe.MatchString(raw) {
		fail("激活码格式不正确")
		return
	}
	if label != "" && !labelRe.MatchString(label) {
		fail("备注名只能包含中英文、数字、空格与 - _ .")
		return
	}

	n, err := s.db.CountUserInstances(ctx, ac.User.ID)
	if err == nil && n >= maxInstancesPerUser {
		fail(fmt.Sprintf("单个账号最多可开通 %d 台实例", maxInstancesPerUser))
		return
	}

	code, err := s.db.CodeByValue(ctx, raw)
	if err != nil {
		s.db.Audit(ctx, ac.User.ID, ac.User.Email, "redeem.fail", "激活码不存在", clientIP(r))
		fail("激活码无效或已被使用")
		return
	}
	if code.UsedBy != 0 {
		fail("激活码无效或已被使用")
		return
	}
	if code.ExpiresAt > 0 && code.ExpiresAt < time.Now().Unix() {
		fail("激活码已过期")
		return
	}

	plan, err := s.db.PlanByID(ctx, code.PlanID)
	if err != nil || !plan.Enabled {
		fail("该激活码对应的套餐已下架")
		return
	}
	if image == "" {
		imgs := plan.ImageList()
		if len(imgs) == 0 {
			fail("套餐未配置可用系统镜像")
			return
		}
		image = imgs[0]
	}
	if !plan.AllowsImage(image) {
		fail("所选系统不在该套餐允许范围内")
		return
	}

	node, err := s.pickNode(ctx, code.NodeID, plan)
	if err != nil {
		fail(err.Error())
		return
	}

	// Claim the code first. The conditional UPDATE is the concurrency guard:
	// two simultaneous redemptions cannot both win.
	claimed, err := s.db.ClaimCode(ctx, code.ID, ac.User.ID)
	if err != nil || !claimed {
		fail("激活码无效或已被使用")
		return
	}

	// Any launch failure must hand the code back.
	inst, rootPW, err := s.launch(ctx, launchSpec{
		user: ac.User, plan: plan, node: node,
		image: image, label: label, codeID: code.ID,
	})
	if err != nil {
		_ = s.db.ReleaseCode(ctx, code.ID)
		fail(err.Error())
		return
	}

	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.create",
		fmt.Sprintf("%s on %s (%s)", inst.Name, node.Name, plan.Name), clientIP(r))

	// The generated root password is shown exactly once.
	p := s.newPage(r, "开通成功", "dashboard")
	p.Data["Inst"] = inst
	p.Data["RootPassword"] = rootPW
	p.Data["SSHHost"] = hostOnly(node.Endpoint)
	s.render(w, r, "redeem_ok.html", p)
}

// pickNode resolves the target node: the code's pin if set, otherwise the
// enabled node with the most headroom.
func (s *Server) pickNode(ctx context.Context, pinned int64, plan *store.Plan) (*store.Node, error) {
	nodeOK := func(n *store.Node) error {
		if needsV6(plan.Mode) && !n.V6Enabled {
			return errors.New("该节点未开启独立 IPv6")
		}
		if needsDV4(plan.Mode) && !n.V4Enabled {
			return errors.New("该节点未开启独立公网 IPv4")
		}
		if plan.InstanceType == "vm" && !n.KVMEnabled {
			return errors.New("该节点未开启 KVM 支持")
		}
		return nil
	}
	if pinned > 0 {
		n, err := s.db.NodeByID(ctx, pinned)
		if err != nil || !n.Enabled {
			return nil, errors.New("该激活码绑定的节点当前不可用")
		}
		if err := nodeOK(n); err != nil {
			return nil, err
		}
		return n, nil
	}
	nodes, err := s.db.ListNodes(ctx, true)
	if err != nil || len(nodes) == 0 {
		return nil, errors.New("暂无可用节点，请联系管理员")
	}
	var best *store.Node
	bestFree := -1
	for _, n := range nodes {
		if nodeOK(n) != nil {
			continue
		}
		free := n.MaxInstances - n.InstanceCount
		if free > bestFree {
			best, bestFree = n, free
		}
	}
	if best == nil || bestFree <= 0 {
		return nil, errors.New("暂无可用节点，请联系管理员")
	}
	return best, nil
}

// launchSpec carries everything needed to materialise an instance once the
// caller has settled entitlement (code claimed or balance debited).
type launchSpec struct {
	user   *store.User
	plan   *store.Plan
	node   *store.Node
	image  string
	label  string
	codeID int64
	// refundCents, when > 0, is returned to the user's balance if the
	// node-side provisioning fails.
	refundCents int64
}

// needsV6/needsDV4/needsNAT classify a plan network mode.
func needsV6(mode string) bool  { return shared.ModeHasDV6(shared.NetMode(mode)) }
func needsDV4(mode string) bool { return shared.ModeHasDV4(shared.NetMode(mode)) }
func needsNAT(mode string) bool { return shared.ModeHasNAT(shared.NetMode(mode)) }

// launch allocates resources, records the instance and starts provisioning
// in the background. Callers roll back their entitlement on error.
//
// Allocation and the instance insert run inside one AllocSection: the picked
// addresses/ports only become visible to the next Allocate once the row is
// written, so the whole pick-then-persist must be a single critical section.
func (s *Server) launch(ctx context.Context, sp launchSpec) (*store.Instance, string, error) {
	var inst *store.Instance
	rootPW := randomPassword(16)
	err := store.AllocSection(func() error {
		alloc, err := s.db.Allocate(ctx, sp.node, store.AllocSpec{
			WantNAT: needsNAT(sp.plan.Mode),
			WantDV4: needsDV4(sp.plan.Mode),
			WantDV6: needsV6(sp.plan.Mode),
			WantVNC: sp.plan.InstanceType == "vm",
			V4Pool:  sp.plan.V4Pool,
		})
		if err != nil {
			return fmt.Errorf("资源分配失败：%v", err)
		}
		name, err := s.uniqueName(ctx)
		if err != nil {
			return errors.New("生成实例名失败")
		}

		instType := sp.plan.InstanceType
		if instType != "vm" {
			instType = "container"
		}
		inst = &store.Instance{
			UserID: sp.user.ID, NodeID: sp.node.ID, PlanID: sp.plan.ID, CodeID: sp.codeID,
			InstanceType: instType,
			Name:         name, Label: sp.label, Image: sp.image, Family: familyOf(sp.image),
			CPU: sp.plan.CPU, MemoryMB: sp.plan.MemoryMB, DiskGB: sp.plan.DiskGB,
			Mode: sp.plan.Mode, NATAddr: alloc.NATAddr, SSHPort: alloc.SSHPort,
			PortFrom: alloc.PortFrom, PortTo: alloc.PortTo, V6Addr: alloc.V6Addr,
			V4Addr:       alloc.V4Addr,
			ExtraBridges: sp.plan.ExtraBridges,
			Status:       "provisioning",
		}
		// VMs get a VNC console: a dedicated host port + an 8-char password
		// (QEMU's built-in VNC auth is DES and caps at 8 characters).
		if instType == "vm" && alloc.VNCPort > 0 {
			inst.VNCPort = alloc.VNCPort
			inst.VNCPass = randomPassword(8)
		}
		if sp.plan.DurationDays > 0 {
			inst.ExpiresAt = time.Now().AddDate(0, 0, sp.plan.DurationDays).Unix()
		}
		inst.RateDownMbps = sp.plan.RateDownMbps
		inst.RateUpMbps = sp.plan.RateUpMbps
		inst.TrafficLimitGB = sp.plan.TrafficGB
		inst.TrafficMode = trafficModeOr(sp.plan.TrafficMode)
		if inst.TrafficLimitGB > 0 {
			inst.TrafficResetAt = time.Now().AddDate(0, 0, 30).Unix()
		}
		instID, err := s.db.CreateInstance(ctx, inst)
		if err != nil {
			return errors.New("创建实例记录失败")
		}
		inst.ID = instID
		return nil
	})
	if err != nil {
		return nil, "", err
	}

	// Image pulls can take minutes, so provision out of band and let the
	// dashboard poll for the result.
	go s.provision(sp.node, inst, rootPW, sp.plan.FeatureList(), sp.plan.KeepSourceIP, sp.refundCents)
	return inst, rootPW, nil
}

// provision drives the node agent and records the outcome.
func (s *Server) provision(node *store.Node, inst *store.Instance, rootPW string, features []string, keepSourceIP bool, refundCents int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	req := buildCreateReq(node, inst, features, keepSourceIP, rootPW)

	if err := agentCreate(ctx, node, req); err != nil {
		log.Printf("provision %s on %s: %v", inst.Name, node.Name, err)
		_ = s.db.SetInstanceStatus(ctx, inst.ID, "error", err.Error())
		// Balance purchases are refunded when the node fails to deliver.
		if refundCents > 0 {
			if _, rerr := s.db.AdjustBalance(ctx, inst.UserID, refundCents,
				"refund", "", "开通失败自动退款 "+inst.Name); rerr == nil {
				s.db.Audit(ctx, 0, "system", "balance.refund",
					fmt.Sprintf("user %d +%d 分 (%s)", inst.UserID, refundCents, inst.Name), "")
			} else {
				log.Printf("refund %s: %v", inst.Name, rerr)
			}
		}
		return
	}
	_ = s.db.SetInstanceStatus(ctx, inst.ID, "running", "")
}

// uniqueName generates an LXD-legal instance name that is free in the panel.
func (s *Server) uniqueName(ctx context.Context) (string, error) {
	const alpha = "abcdefghijkmnpqrstuvwxyz23456789"
	for attempt := 0; attempt < 12; attempt++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = alpha[int(b[i])%len(alpha)]
		}
		name := "cub-" + string(b)
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM instances WHERE name = ?`, name).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return name, nil
		}
	}
	return "", errors.New("could not allocate a unique name")
}

// ---------- tenant JSON API ----------

// handleAPIState proxies live state from the node, falling back to the cached
// record when the node is unreachable.
func (s *Server) handleAPIState(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	if inst.Status == "provisioning" {
		s.jsonOK(w, map[string]any{"status": "provisioning", "name": inst.Name})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	st, err := agentState(ctx, node, inst.Name)
	if err != nil {
		s.jsonOK(w, map[string]any{"status": inst.Status, "name": inst.Name, "stale": true})
		return
	}
	// Keep the cached status roughly in step for list views. Administrative
	// states (expired/overquota) must not be clobbered by live polling.
	if cur := strings.ToLower(st.Status); cur != inst.Status &&
		inst.Status != "expired" && inst.Status != "overquota" {
		_ = s.db.SetInstanceStatus(context.Background(), inst.ID, cur, "")
	}
	s.jsonOK(w, map[string]any{
		"name":     inst.Name,
		"status":   strings.ToLower(st.Status),
		"ipv4":     st.IPv4,
		"ipv6":     st.IPv6,
		"mem_used": st.MemUsed,
		"mem_max":  int64(inst.MemoryMB) * 1024 * 1024,
		"disk":     st.DiskUse,
		"net_rx":   st.NetRx,
		"net_tx":   st.NetTx,
		"uptime":   st.Uptime,
	})
}

// handleAPIAction performs a lifecycle action on an owned instance.
func (s *Server) handleAPIAction(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	action := formStr(r, "action", 16)
	switch action {
	case "start", "stop", "restart":
	default:
		s.jsonErr(w, http.StatusBadRequest, "不支持的操作")
		return
	}
	if inst.Status == "expired" && action != "stop" {
		s.jsonErr(w, http.StatusForbidden, "实例已到期，请续期后再操作")
		return
	}
	if inst.Status == "overquota" && action != "stop" {
		s.jsonErr(w, http.StatusForbidden, "流量已超限，请等待下月重置或联系管理员")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := agentAction(ctx, node, inst.Name, action, action == "stop"); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "操作失败："+err.Error())
		return
	}

	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "instance."+action, inst.Name, clientIP(r))
	newStatus := map[string]string{"start": "running", "stop": "stopped", "restart": "running"}[action]
	_ = s.db.SetInstanceStatus(context.Background(), inst.ID, newStatus, "")
	s.jsonOK(w, map[string]any{"ok": true, "status": newStatus})
}

// handleAPIPassword resets the container's root password.
func (s *Server) handleAPIPassword(w http.ResponseWriter, r *http.Request) {
	inst, node, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	if inst.Status != "running" {
		s.jsonErr(w, http.StatusConflict, "请先启动实例再重置密码")
		return
	}
	pw := randomPassword(16)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := agentPassword(ctx, node, inst.Name, pw); err != nil {
		s.jsonErr(w, http.StatusBadGateway, "重置失败："+err.Error())
		return
	}
	ac := userFrom(r)
	s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "instance.password", inst.Name, clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true, "password": pw})
}

// handleAPILabel renames an instance in the UI.
func (s *Server) handleAPILabel(w http.ResponseWriter, r *http.Request) {
	inst, _, ok := s.ownedInstance(w, r)
	if !ok {
		return
	}
	label := formStr(r, "label", 32)
	if label != "" && !labelRe.MatchString(label) {
		s.jsonErr(w, http.StatusBadRequest, "备注名含有不允许的字符")
		return
	}
	if err := s.db.SetInstanceLabel(r.Context(), inst.ID, label); err != nil {
		s.jsonErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.jsonOK(w, map[string]any{"ok": true, "label": label})
}

// ---------- helpers ----------

// familyOf maps an image alias onto a provisioning recipe.
func familyOf(image string) string {
	if strings.HasPrefix(strings.ToLower(image), "alpine") {
		return "alpine"
	}
	return "debian"
}

// hostOnly strips scheme and port from a node endpoint, giving the address a
// tenant would SSH to.
func hostOnly(endpoint string) string {
	h, err := parseURLHost(endpoint)
	if err != nil || h == "" {
		return ""
	}
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.Trim(h, "[]")
}

// buildCreateReq assembles the agent CreateRequest for an instance on a
// given node. Shared by first provisioning and by migration reconfigure.
func buildCreateReq(node *store.Node, inst *store.Instance, features []string, keepSourceIP bool, rootPW string) *shared.CreateRequest {
	req := &shared.CreateRequest{
		Name: inst.Name, InstanceType: inst.InstanceType,
		Image: inst.Image, Family: shared.OSFamily(inst.Family),
		CPU: inst.CPU, MemoryMB: inst.MemoryMB, DiskGB: inst.DiskGB,
		StoragePool: node.StoragePool,
		Mode:        shared.NetMode(inst.Mode), Features: features,
		RateDownMbps: inst.RateDownMbps, RateUpMbps: inst.RateUpMbps,
		ExtraBridges: splitCSV(inst.ExtraBridges),
		VNCPort:      inst.VNCPort, VNCPass: inst.VNCPass,
		KeepSourceIP: keepSourceIP,
		NATBridge:    node.NATBridge, NATAddr: inst.NATAddr, NATManaged: node.NATManaged,
		SSHPort: inst.SSHPort, PortFrom: inst.PortFrom, PortTo: inst.PortTo,
		DNS:          node.DNS,
		RootPassword: rootPW,
	}
	if !node.NATManaged && inst.NATAddr != "" {
		req.NATGW = node.NATGW
		if req.NATGW == "" {
			req.NATGW = firstHostV4(node.NATSubnet)
		}
		req.NATPrefix = v4PrefixLen(node.NATSubnet)
	}
	if needsV6(inst.Mode) {
		req.V6Bridge = node.V6Bridge
		req.V6Addr = inst.V6Addr
		req.V6Prefix = prefixLen(node.V6CIDR)
		req.V6GW = node.V6GW
	}
	if needsDV4(inst.Mode) {
		req.V4Bridge = node.V4Bridge
		req.V4Addr = inst.V4Addr
		req.V4Prefix = v4PrefixLen(node.V4CIDR)
		req.V4GW = node.V4GW
	}
	return req
}

// splitCSV splits a trimmed comma list, dropping empties.
func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// trafficModeOr normalises a traffic billing mode, defaulting to both-ways.
func trafficModeOr(m string) string {
	switch m {
	case "up", "down":
		return m
	default:
		return "both"
	}
}

// v4PrefixLen reads an IPv4 CIDR's prefix length, defaulting to 24.
func v4PrefixLen(cidr string) int {
	if pfx, err := netip.ParsePrefix(cidr); err == nil && pfx.Addr().Is4() {
		return pfx.Bits()
	}
	return 24
}

// firstHostV4 returns the first host address of an IPv4 subnet — the
// conventional gateway on a shared bridge.
func firstHostV4(cidr string) string {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil || !pfx.Addr().Is4() {
		return ""
	}
	return pfx.Masked().Addr().Next().String()
}

// prefixLen reads the prefix length out of a CIDR string.
func prefixLen(cidr string) int {
	i := strings.LastIndex(cidr, "/")
	if i < 0 {
		return 64
	}
	n := 0
	for _, c := range cidr[i+1:] {
		if c < '0' || c > '9' {
			return 64
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 || n > 128 {
		return 64
	}
	return n
}
