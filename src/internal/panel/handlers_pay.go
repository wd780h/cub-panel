// User-facing recharge: create orders and settle them.
//
// Alipay / WeChat go through an epay-compatible gateway (the de-facto standard
// for small panels): the operator plugs in their own epay site URL, merchant
// id and key under 网站设置. USDT is address-based: the user pays to the
// configured wallet and submits the txid for admin confirmation. Either way
// crediting runs through the idempotent order → balance path.
package panel

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"cubpanel/internal/store"
)

// payCfg reads the current payment configuration from settings.
type payCfg struct {
	epayURL, epayPID, epayKey string
	alipay, wxpay             bool
	usdtOn                    bool
	usdtAddr, usdtNet         string
	usdtRate                  int64 // CNY cents per 1 USDT
}

func (s *Server) payConfig(r *http.Request) payCfg {
	ctx := r.Context()
	g := func(k string) string { return s.db.Setting(ctx, k, "") }
	rate, _ := parseMoney(g("pay_usdt_rate"))
	return payCfg{
		epayURL: g("pay_epay_url"), epayPID: g("pay_epay_pid"), epayKey: g("pay_epay_key"),
		alipay: g("pay_alipay") == "1", wxpay: g("pay_wxpay") == "1",
		usdtOn: g("pay_usdt") == "1", usdtAddr: g("pay_usdt_addr"), usdtNet: nonEmptyStr(g("pay_usdt_net"), "TRC20"),
		usdtRate: rate,
	}
}

func nonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// enabledMethods lists the payment methods currently switched on and usable.
func (c payCfg) enabledMethods() []string {
	var m []string
	if c.epayURL != "" && c.epayPID != "" && c.epayKey != "" {
		if c.alipay {
			m = append(m, "alipay")
		}
		if c.wxpay {
			m = append(m, "wxpay")
		}
	}
	if c.usdtOn && c.usdtAddr != "" && c.usdtRate > 0 {
		m = append(m, "usdt")
	}
	return m
}

// ---------- recharge page ----------

func (s *Server) handleRechargePage(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "充值", "recharge")
	cfg := s.payConfig(r)
	if u, err := s.db.UserByID(r.Context(), p.User.ID); err == nil {
		p.Data["Balance"] = u.BalanceCents
	}
	p.Data["Methods"] = cfg.enabledMethods()
	p.Data["USDTRate"] = cfg.usdtRate
	orders, _ := s.db.ListUserOrders(r.Context(), p.User.ID, 10)
	p.Data["Orders"] = orders
	switch r.URL.Query().Get("ok") {
	case "paid":
		p.Flash = "支付完成，余额稍后到账（如未到账请刷新或联系管理员）。"
	case "submitted":
		p.Flash = "已提交交易号，等待管理员确认到账。"
	}
	s.render(w, r, "recharge.html", p)
}

// newOrderNo returns a unique station order number.
func newOrderNo() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "R" + hex.EncodeToString(b[:])
}

// handleRechargeCreate makes an order and, for epay methods, redirects to the
// gateway; for USDT it shows the payment instructions.
func (s *Server) handleRechargeCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	cfg := s.payConfig(r)

	method := formStr(r, "method", 8)
	amount, err := parseMoney(r.FormValue("amount"))
	if err != nil || amount < 100 || amount > 100_000_00 {
		s.renderError(w, r, http.StatusBadRequest, "充值金额需在 1 – 100000 元之间")
		return
	}
	allowed := false
	for _, m := range cfg.enabledMethods() {
		if m == method {
			allowed = true
		}
	}
	if !allowed {
		s.renderError(w, r, http.StatusBadRequest, "该支付方式当前不可用")
		return
	}

	o := &store.Order{OrderNo: newOrderNo(), UserID: ac.User.ID, AmountCents: amount, Method: method}
	id, err := s.db.CreateOrder(ctx, o)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "创建订单失败")
		return
	}
	o.ID = id
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "recharge.order",
		fmt.Sprintf("%s %s 元 (%s)", o.OrderNo, fmtMoney(amount), method), clientIP(r))

	if method == "usdt" {
		http.Redirect(w, r, "/app/recharge/usdt/"+fmt.Sprint(o.ID), http.StatusSeeOther)
		return
	}
	// epay: build the signed payment URL and bounce the user there.
	http.Redirect(w, r, s.epaySubmitURL(r, cfg, o), http.StatusSeeOther)
}

// ---------- epay (alipay / wxpay) ----------

// epaySign computes the epay MD5 signature over sorted non-empty params.
func epaySign(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || k == "sign_type" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(k + "=" + params[k])
	}
	sb.WriteString(key)
	sum := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// epaySubmitURL returns the gateway URL the browser is redirected to.
func (s *Server) epaySubmitURL(r *http.Request, cfg payCfg, o *store.Order) string {
	base := strings.TrimRight(cfg.epayURL, "/")
	origin := siteOrigin(r)
	params := map[string]string{
		"pid":          cfg.epayPID,
		"type":         o.Method, // epay uses "alipay" / "wxpay"
		"out_trade_no": o.OrderNo,
		"notify_url":   origin + "/pay/epay/notify",
		"return_url":   origin + "/app/recharge?ok=paid",
		"name":         "余额充值 " + o.OrderNo,
		"money":        fmtMoney(o.AmountCents),
	}
	params["sign"] = epaySign(params, cfg.epayKey)
	params["sign_type"] = "MD5"

	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return base + "/submit.php?" + q.Encode()
}

// handleEpayNotify is the gateway's async callback. It verifies the signature
// and credits the order idempotently, then replies with the literal "success"
// epay expects.
func (s *Server) handleEpayNotify(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	cfg := s.payConfig(r)
	if cfg.epayKey == "" {
		http.Error(w, "fail", http.StatusServiceUnavailable)
		return
	}
	params := map[string]string{}
	for k := range r.Form {
		params[k] = r.Form.Get(k)
	}
	if params["trade_status"] != "TRADE_SUCCESS" && params["trade_status"] != "" {
		// Some epay builds omit trade_status; only reject explicit non-success.
		_, _ = w.Write([]byte("fail"))
		return
	}
	want := epaySign(params, cfg.epayKey)
	if !strings.EqualFold(want, params["sign"]) {
		_, _ = w.Write([]byte("fail"))
		return
	}
	orderNo := params["out_trade_no"]
	if orderNo == "" {
		_, _ = w.Write([]byte("fail"))
		return
	}
	if _, err := s.db.MarkOrderPaid(r.Context(), orderNo, params["trade_no"]); err != nil {
		_, _ = w.Write([]byte("fail"))
		return
	}
	s.db.Audit(r.Context(), 0, "gateway", "recharge.paid", orderNo+" via epay", clientIP(r))
	_, _ = w.Write([]byte("success"))
}

// ---------- usdt ----------

func (s *Server) handleUSDTPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	o, err := s.db.OrderByID(ctx, pathIDMust(r))
	if err != nil || o.UserID != ac.User.ID {
		s.renderError(w, r, http.StatusNotFound, "订单不存在")
		return
	}
	cfg := s.payConfig(r)
	p := s.newPage(r, "USDT 充值", "recharge")
	p.Data["Order"] = o
	p.Data["Addr"] = cfg.usdtAddr
	p.Data["Net"] = cfg.usdtNet
	// USDT amount = CNY / rate, rounded to 2 decimals.
	if cfg.usdtRate > 0 {
		p.Data["USDT"] = fmt.Sprintf("%.2f", float64(o.AmountCents)/float64(cfg.usdtRate))
	}
	s.render(w, r, "recharge_usdt.html", p)
}

func (s *Server) handleUSDTSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	o, err := s.db.OrderByID(ctx, formInt64(r, "id"))
	if err != nil || o.UserID != ac.User.ID {
		s.renderError(w, r, http.StatusNotFound, "订单不存在")
		return
	}
	txid := formStr(r, "txid", 128)
	if len(txid) < 8 {
		s.renderError(w, r, http.StatusBadRequest, "请填写有效的交易哈希（TxID）")
		return
	}
	_ = s.db.SetOrderTxID(ctx, o.ID, txid)
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "recharge.usdt.submit", o.OrderNo+" "+txid, clientIP(r))
	http.Redirect(w, r, "/app/recharge?ok=submitted", http.StatusSeeOther)
}

// ---------- helpers ----------

// siteOrigin reconstructs the public origin for callback URLs.
func siteOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// pathIDMust reads {id}, returning 0 on failure (callers check ownership).
func pathIDMust(r *http.Request) int64 {
	v, _ := pathID(r)
	return v
}
