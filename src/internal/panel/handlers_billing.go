// Balance-based provisioning and the server-to-server recharge API.
package panel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"cubpanel/internal/store"
)

// ---------- money helpers ----------

// fmtMoney renders cents as a decimal amount ("12.50").
func fmtMoney(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// parseMoney converts a decimal amount ("12.5", "-3", "0.05") into cents
// without going through floating point.
func parseMoney(s string) (int64, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return 0, errors.New("empty amount")
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 2 {
		return 0, errors.New("at most 2 decimal places")
	}
	for len(frac) < 2 {
		frac += "0"
	}
	var cents int64
	for _, c := range whole + frac {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		cents = cents*10 + int64(c-'0')
		if cents > 1<<40 {
			return 0, errors.New("amount too large")
		}
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}

// ---------- balance deployment (tenant) ----------

// handleDeployPage lists balance-purchasable plans.
// deployPage builds the merged "创建实例" page envelope (balance plans +
// activation-code panel). Shared by the page handler and the redeem/deploy
// failure paths so a rejected submission comes back on the same screen.
func (s *Server) deployPage(r *http.Request) *page {
	ctx := r.Context()
	p := s.newPage(r, "创建实例", "deploy")
	if u, err := s.db.UserByID(ctx, p.User.ID); err == nil {
		p.Data["Balance"] = u.BalanceCents
	}
	plans, _ := s.db.ListPlans(ctx, true)
	// Offer only images a node able to host the plan can actually deliver, so
	// a typo'd or withdrawn alias never reaches a buyer. A plan left with no
	// deliverable image is not purchasable at all.
	type buyablePlan struct {
		*store.Plan
		Images []string
	}
	var buyable []buyablePlan
	for _, pl := range plans {
		if pl.PriceCents <= 0 {
			continue
		}
		imgs := s.planImages(ctx, pl)
		if len(imgs) == 0 {
			continue
		}
		buyable = append(buyable, buyablePlan{Plan: pl, Images: imgs})
	}
	p.Data["Plans"] = buyable

	// The activation-code form cannot know which plan a code belongs to until
	// it is redeemed, so it offers the union of every deliverable image; the
	// redeem handler still enforces the code's own plan.
	seen := map[string]bool{}
	var redeemImgs []string
	for _, bp := range buyable {
		for _, a := range bp.Images {
			if !seen[a] {
				seen[a] = true
				redeemImgs = append(redeemImgs, a)
			}
		}
	}
	for _, pl := range plans {
		if pl.PriceCents > 0 {
			continue // already covered above
		}
		for _, a := range s.planImages(ctx, pl) {
			if !seen[a] {
				seen[a] = true
				redeemImgs = append(redeemImgs, a)
			}
		}
	}
	sort.Strings(redeemImgs)
	p.Data["RedeemImages"] = redeemImgs
	txs, _ := s.db.ListTransactions(ctx, p.User.ID, 10)
	p.Data["Transactions"] = txs
	return p
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request) {
	p := s.deployPage(r)
	p.Data["Code"] = formStr(r, "code", 64)
	s.render(w, r, "deploy.html", p)
}

// handleDeployPost debits the balance and provisions an instance.
func (s *Server) handleDeployPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	label := formStr(r, "label", 32)
	image := formStr(r, "image", 64)

	fail := func(msg string) {
		p := s.deployPage(r)
		p.Error = msg
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "deploy.html", p)
	}

	if label != "" && !labelRe.MatchString(label) {
		fail("备注名只能包含中英文、数字、空格与 - _ .")
		return
	}
	plan, err := s.db.PlanByID(ctx, formInt64(r, "plan_id"))
	if err != nil || !plan.Enabled || plan.PriceCents <= 0 {
		fail("该套餐不可用余额开通")
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
	if n, err := s.db.CountUserInstances(ctx, ac.User.ID); err == nil && n >= maxInstancesPerUser {
		fail(fmt.Sprintf("单个账号最多可开通 %d 台实例", maxInstancesPerUser))
		return
	}
	node, err := s.pickNode(ctx, 0, plan)
	if err != nil {
		fail(err.Error())
		return
	}

	// Debit first — the conditional UPDATE inside AdjustBalance is the
	// concurrency guard, mirroring how codes are claimed.
	if _, err := s.db.AdjustBalance(ctx, ac.User.ID, -plan.PriceCents,
		"purchase", "", "余额开通 "+plan.Name); err != nil {
		if errors.Is(err, store.ErrInsufficient) {
			fail(fmt.Sprintf("余额不足：需要 %s 元，请先充值", fmtMoney(plan.PriceCents)))
			return
		}
		fail("扣款失败，请稍后再试")
		return
	}

	// Any launch failure must give the money back.
	inst, rootPW, err := s.launch(ctx, launchSpec{
		user: ac.User, plan: plan, node: node,
		image: image, label: label, refundCents: plan.PriceCents,
	})
	if err != nil {
		_, _ = s.db.AdjustBalance(ctx, ac.User.ID, plan.PriceCents,
			"refund", "", "开通失败退款 "+plan.Name)
		fail(err.Error())
		return
	}

	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.purchase",
		fmt.Sprintf("%s on %s (%s, %s 元)", inst.Name, node.Name, plan.Name, fmtMoney(plan.PriceCents)), clientIP(r))

	p := s.newPage(r, "开通成功", "dashboard")
	p.Data["Inst"] = inst
	p.Data["RootPassword"] = rootPW
	p.Data["SSHHost"] = hostOnly(node.Endpoint)
	s.render(w, r, "redeem_ok.html", p)
}

// ---------- elastic upgrade (tenant) ----------

// upgradeCandidates lists plans an instance may move up to: same network
// mode, balance-purchasable, no smaller in any dimension, and costlier than
// what was already paid.
func (s *Server) upgradeCandidates(ctx context.Context, inst *store.Instance) []*store.Plan {
	plans, _ := s.db.ListPlans(ctx, true)
	oldPrice := s.instancePlanPrice(ctx, inst)
	var out []*store.Plan
	for _, pl := range plans {
		if pl.ID == inst.PlanID || pl.PriceCents <= oldPrice || pl.Mode != inst.Mode {
			continue
		}
		if pl.InstanceType != inst.InstanceType && !(pl.InstanceType == "container" && inst.InstanceType == "") {
			continue
		}
		if pl.CPU < inst.CPU || pl.MemoryMB < inst.MemoryMB || pl.DiskGB < inst.DiskGB {
			continue
		}
		out = append(out, pl)
	}
	return out
}

// instancePlanPrice reads the price of the plan the instance currently sits
// on; instances from deleted or code-only plans count as 0.
func (s *Server) instancePlanPrice(ctx context.Context, inst *store.Instance) int64 {
	if inst.PlanID <= 0 {
		return 0
	}
	if pl, err := s.db.PlanByID(ctx, inst.PlanID); err == nil {
		return pl.PriceCents
	}
	return 0
}

// handleInstanceUpgrade moves an owned instance onto a bigger plan, paying
// the price difference from the balance and resizing in place.
func (s *Server) handleInstanceUpgrade(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "参数无效")
		return
	}
	ac := userFrom(r)
	inst, err := s.db.InstanceByID(ctx, id)
	if err != nil || (inst.UserID != ac.User.ID && !ac.User.IsAdmin) {
		s.renderError(w, r, http.StatusNotFound, "实例不存在")
		return
	}
	back := fmt.Sprintf("/app/instance/%d", inst.ID)
	fail := func(code string) { http.Redirect(w, r, back+"?err="+code, http.StatusSeeOther) }

	if inst.Status == "provisioning" || inst.Status == "error" {
		fail("upgrade_state")
		return
	}
	plan, err := s.db.PlanByID(ctx, formInt64(r, "plan_id"))
	instType := inst.InstanceType
	if instType == "" {
		instType = "container"
	}
	if err != nil || !plan.Enabled || plan.PriceCents <= 0 ||
		plan.Mode != inst.Mode || plan.InstanceType != instType ||
		plan.CPU < inst.CPU || plan.MemoryMB < inst.MemoryMB || plan.DiskGB < inst.DiskGB {
		fail("upgrade_bad")
		return
	}
	diff := plan.PriceCents - s.instancePlanPrice(ctx, inst)
	if diff <= 0 {
		fail("upgrade_bad")
		return
	}
	node, err := s.db.NodeByID(ctx, inst.NodeID)
	if err != nil {
		fail("upgrade_fail")
		return
	}

	if _, err := s.db.AdjustBalance(ctx, ac.User.ID, -diff,
		"purchase", "", fmt.Sprintf("升级 %s → %s", inst.Name, plan.Name)); err != nil {
		if errors.Is(err, store.ErrInsufficient) {
			fail("upgrade_insufficient")
			return
		}
		fail("upgrade_fail")
		return
	}

	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := agentResize(dctx, node, inst.Name, plan.CPU, plan.MemoryMB, plan.DiskGB,
		plan.RateDownMbps, plan.RateUpMbps); err != nil {
		_, _ = s.db.AdjustBalance(ctx, ac.User.ID, diff, "refund", "", "升级失败退款 "+inst.Name)
		fail("upgrade_fail")
		return
	}
	_ = s.db.ResizeInstance(ctx, inst.ID, plan.CPU, plan.MemoryMB, plan.DiskGB,
		plan.RateDownMbps, plan.RateUpMbps, plan.ID)
	// The new plan's traffic terms take over (usage carries across).
	_ = s.db.SetInstanceTraffic(ctx, inst.ID, plan.TrafficGB, trafficModeOr(plan.TrafficMode),
		time.Now().AddDate(0, 0, 30).Unix())

	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "instance.upgrade",
		fmt.Sprintf("%s → %s (+%s 元)", inst.Name, plan.Name, fmtMoney(diff)), clientIP(r))
	http.Redirect(w, r, back+"?ok=upgrade", http.StatusSeeOther)
}

// ---------- admin: balance adjustment & API key ----------

// handleAdminUserBalance credits or debits an account manually.
func (s *Server) handleAdminUserBalance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)

	target, err := s.db.UserByID(ctx, formInt64(r, "id"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "用户不存在")
		return
	}
	cents, err := parseMoney(r.FormValue("amount"))
	if err != nil || cents == 0 {
		s.renderError(w, r, http.StatusBadRequest, "金额格式不正确（例如 10 或 -5.50）")
		return
	}
	note := formStr(r, "note", 100)
	if note == "" {
		note = "管理员调整"
	}

	balance, err := s.db.AdjustBalance(ctx, target.ID, cents, "admin", "", note)
	if err != nil {
		if errors.Is(err, store.ErrInsufficient) {
			s.renderError(w, r, http.StatusBadRequest, "扣款金额超过该用户当前余额")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "调整失败")
		return
	}
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "balance.adjust",
		fmt.Sprintf("%s %+d 分 → %s 元", target.Email, cents, fmtMoney(balance)), clientIP(r))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// handleAdminAPIKey rotates the recharge API key.
func (s *Server) handleAdminAPIKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	key := hex.EncodeToString(b[:])
	if err := s.db.SetSetting(ctx, "recharge_api_key", key); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "保存密钥失败")
		return
	}
	ac := userFrom(r)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "apikey.rotate", "recharge API", clientIP(r))
	http.Redirect(w, r, "/admin/users?ok=key", http.StatusSeeOther)
}

// ---------- recharge API (server-to-server) ----------

// apiKeyOK checks the Bearer token against the stored recharge API key in
// constant time. An unset key disables the API entirely.
func (s *Server) apiKeyOK(r *http.Request) bool {
	key := s.db.Setting(r.Context(), "recharge_api_key", "")
	if len(key) < 32 {
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(key)) == 1
}

// handleAPIRecharge credits a user's balance. Idempotent on ref: replaying a
// processed order returns the current balance instead of double-crediting.
//
//	POST /api/v1/recharge
//	Authorization: Bearer <key>
//	{"email": "user@example.com", "amount_cents": 1000, "ref": "order-123", "note": "alipay"}
func (s *Server) handleAPIRecharge(w http.ResponseWriter, r *http.Request) {
	if !s.apiKeyOK(r) {
		s.jsonErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Email       string `json:"email"`
		UserID      int64  `json:"user_id"`
		AmountCents int64  `json:"amount_cents"`
		Ref         string `json:"ref"`
		Note        string `json:"note"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &req) != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed json body")
		return
	}
	if req.AmountCents <= 0 || req.AmountCents > 100_000_00 {
		s.jsonErr(w, http.StatusBadRequest, "amount_cents must be 1..10000000")
		return
	}
	req.Ref = strings.TrimSpace(req.Ref)
	if req.Ref == "" || len(req.Ref) > 64 {
		s.jsonErr(w, http.StatusBadRequest, "ref is required (max 64 chars) and makes the call idempotent")
		return
	}
	if len(req.Note) > 200 {
		req.Note = req.Note[:200]
	}

	ctx := r.Context()
	var user *store.User
	if req.UserID > 0 {
		user, err = s.db.UserByID(ctx, req.UserID)
	} else {
		user, err = s.db.UserByEmail(ctx, req.Email)
	}
	if err != nil {
		s.jsonErr(w, http.StatusNotFound, "user not found")
		return
	}

	balance, err := s.db.AdjustBalance(ctx, user.ID, req.AmountCents, "recharge", req.Ref, req.Note)
	if errors.Is(err, store.ErrDuplicateRef) {
		// Same order delivered twice (callback retry): report current state.
		if u, uerr := s.db.UserByID(ctx, user.ID); uerr == nil {
			s.jsonOK(w, map[string]any{"ok": true, "duplicated": true,
				"user_id": u.ID, "balance_cents": u.BalanceCents})
			return
		}
		s.jsonErr(w, http.StatusConflict, "duplicate ref")
		return
	}
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, "recharge failed")
		return
	}
	s.db.Audit(ctx, 0, "api", "balance.recharge",
		fmt.Sprintf("%s +%s 元 (ref %s)", user.Email, fmtMoney(req.AmountCents), req.Ref), clientIP(r))
	s.jsonOK(w, map[string]any{"ok": true, "user_id": user.ID, "balance_cents": balance})
}

// handleAPIBalanceQuery reports a user's balance.
//
//	GET /api/v1/balance?email=user@example.com
func (s *Server) handleAPIBalanceQuery(w http.ResponseWriter, r *http.Request) {
	if !s.apiKeyOK(r) {
		s.jsonErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, err := s.db.UserByEmail(r.Context(), formStr(r, "email", 200))
	if err != nil {
		s.jsonErr(w, http.StatusNotFound, "user not found")
		return
	}
	s.jsonOK(w, map[string]any{"user_id": user.ID, "email": user.Email, "balance_cents": user.BalanceCents})
}
