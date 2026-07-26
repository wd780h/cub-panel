// Package panel is the master web application: the public storefront, the
// tenant dashboard and the administration backend.
package panel

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cubpanel/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

// Config is the panel's runtime configuration.
type Config struct {
	Listen        string
	DBPath        string
	SiteName      string
	SecureCookies bool
	TrustProxy    bool
	AllowSignup   bool
}

// Server is the panel web application.
type Server struct {
	cfg    Config
	db     *store.DB
	tmpl   *template.Template
	limits struct {
		login  *limiter
		signup *limiter
		redeem *limiter
		action *limiter
	}
}

// New builds the panel server and parses templates.
func New(cfg Config, db *store.DB) (*Server, error) {
	trustProxy = cfg.TrustProxy

	s := &Server{cfg: cfg, db: db}
	s.limits.login = newLimiter(8, 10*time.Minute)
	s.limits.signup = newLimiter(5, time.Hour)
	s.limits.redeem = newLimiter(10, 10*time.Minute)
	s.limits.action = newLimiter(120, time.Minute)

	t, err := template.New("").Funcs(s.funcMap()).ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = t
	return s, nil
}

// Handler returns the fully routed, middleware-wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(staticFS, "static")
	fileSrv := http.FileServer(http.FS(sub))
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheStatic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fileSrv.ServeHTTP(w, r)
		}))))

	// Public
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.rateLimit(s.limits.login, s.handleLoginPost))
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("POST /register", s.rateLimit(s.limits.signup, s.handleRegisterPost))
	mux.HandleFunc("POST /logout", s.requireUser(s.csrfGuard(s.handleLogout)))

	// Tenant
	mux.HandleFunc("GET /app", s.requireUser(s.handleDashboard))
	mux.HandleFunc("GET /app/redeem", s.requireUser(s.handleRedeemPage))
	mux.HandleFunc("POST /app/redeem", s.rateLimit(s.limits.redeem, s.requireUser(s.csrfGuard(s.handleRedeemPost))))
	mux.HandleFunc("GET /app/instance/{id}", s.requireUser(s.handleInstancePage))
	mux.HandleFunc("POST /app/instance/{id}/upgrade", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleInstanceUpgrade))))
	mux.HandleFunc("GET /app/console/{id}", s.requireUser(s.handleConsolePage))
	mux.HandleFunc("GET /app/deploy", s.requireUser(s.handleDeployPage))
	mux.HandleFunc("POST /app/deploy", s.rateLimit(s.limits.redeem, s.requireUser(s.csrfGuard(s.handleDeployPost))))
	mux.HandleFunc("GET /app/recharge", s.requireUser(s.handleRechargePage))
	mux.HandleFunc("POST /app/recharge", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleRechargeCreate))))
	mux.HandleFunc("GET /app/recharge/usdt/{id}", s.requireUser(s.handleUSDTPage))
	mux.HandleFunc("POST /app/recharge/usdt", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleUSDTSubmit))))
	mux.HandleFunc("GET /app/settings", s.requireUser(s.handleAccountPage))
	mux.HandleFunc("POST /app/password", s.requireUser(s.csrfGuard(s.handleAccountPassword)))

	// Tenant JSON API
	mux.HandleFunc("GET /api/instances/{id}/state", s.requireUser(s.handleAPIState))
	mux.HandleFunc("POST /api/instances/{id}/action", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleAPIAction))))
	mux.HandleFunc("POST /api/instances/{id}/password", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleAPIPassword))))
	mux.HandleFunc("POST /api/instances/{id}/label", s.requireUser(s.csrfGuard(s.handleAPILabel)))
	mux.HandleFunc("GET /api/instances/{id}/snapshots", s.requireUser(s.handleAPISnapshotList))
	mux.HandleFunc("POST /api/instances/{id}/snapshots", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleAPISnapshotCreate))))
	mux.HandleFunc("POST /api/instances/{id}/snapshots/restore", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleAPISnapshotRestore))))
	mux.HandleFunc("POST /api/instances/{id}/snapshots/delete", s.requireUser(s.csrfGuard(s.handleAPISnapshotDelete)))
	mux.HandleFunc("GET /api/instances/{id}/isos", s.requireUser(s.handleAPIISOList))
	mux.HandleFunc("POST /api/instances/{id}/iso/attach", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleAPIISOAttach))))
	mux.HandleFunc("POST /api/instances/{id}/iso/detach", s.rateLimit(s.limits.action, s.requireUser(s.csrfGuard(s.handleAPIISODetach))))
	mux.HandleFunc("GET /ws/console/{id}", s.requireUser(s.handleConsoleWS))

	// Server-to-server billing API (Bearer key auth, no session/CSRF).
	mux.HandleFunc("POST /api/v1/recharge", s.rateLimit(s.limits.action, s.handleAPIRecharge))
	mux.HandleFunc("GET /api/v1/balance", s.rateLimit(s.limits.action, s.handleAPIBalanceQuery))
	mux.HandleFunc("POST /pay/epay/notify", s.handleEpayNotify)
	mux.HandleFunc("GET /pay/epay/notify", s.handleEpayNotify)

	// Admin
	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdminHome))
	mux.HandleFunc("GET /admin/nodes", s.requireAdmin(s.handleAdminNodes))
	mux.HandleFunc("POST /admin/nodes", s.requireAdmin(s.csrfGuard(s.handleAdminNodeSave)))
	mux.HandleFunc("POST /admin/nodes/delete", s.requireAdmin(s.csrfGuard(s.handleAdminNodeDelete)))
	mux.HandleFunc("POST /admin/nodes/probe", s.requireAdmin(s.csrfGuard(s.handleAdminNodeProbe)))
	mux.HandleFunc("GET /admin/plans", s.requireAdmin(s.handleAdminPlans))
	mux.HandleFunc("POST /admin/plans", s.requireAdmin(s.csrfGuard(s.handleAdminPlanSave)))
	mux.HandleFunc("POST /admin/plans/delete", s.requireAdmin(s.csrfGuard(s.handleAdminPlanDelete)))
	mux.HandleFunc("GET /admin/images", s.requireAdmin(s.handleAdminImages))
	mux.HandleFunc("POST /admin/images/pull", s.requireAdmin(s.csrfGuard(s.handleAdminImagePull)))
	mux.HandleFunc("POST /admin/images/delete", s.requireAdmin(s.csrfGuard(s.handleAdminImageDelete)))
	mux.HandleFunc("POST /admin/isos/pull", s.requireAdmin(s.csrfGuard(s.handleAdminISOPull)))
	mux.HandleFunc("POST /admin/isos/delete", s.requireAdmin(s.csrfGuard(s.handleAdminISODelete)))
	mux.HandleFunc("GET /admin/codes", s.requireAdmin(s.handleAdminCodes))
	mux.HandleFunc("POST /admin/codes", s.requireAdmin(s.csrfGuard(s.handleAdminCodeGen)))
	mux.HandleFunc("POST /admin/codes/delete", s.requireAdmin(s.csrfGuard(s.handleAdminCodeDelete)))
	mux.HandleFunc("GET /admin/users", s.requireAdmin(s.handleAdminUsers))
	mux.HandleFunc("POST /admin/users", s.requireAdmin(s.csrfGuard(s.handleAdminUserAction)))
	mux.HandleFunc("POST /admin/users/balance", s.requireAdmin(s.csrfGuard(s.handleAdminUserBalance)))
	mux.HandleFunc("POST /admin/apikey", s.requireAdmin(s.csrfGuard(s.handleAdminAPIKey)))
	mux.HandleFunc("GET /admin/instances", s.requireAdmin(s.handleAdminInstances))
	mux.HandleFunc("POST /admin/instances/delete", s.requireAdmin(s.csrfGuard(s.handleAdminInstanceDelete)))
	mux.HandleFunc("POST /admin/instances/extend", s.requireAdmin(s.csrfGuard(s.handleAdminInstanceExtend)))
	mux.HandleFunc("POST /admin/instances/resize", s.requireAdmin(s.csrfGuard(s.handleAdminInstanceResize)))
	mux.HandleFunc("POST /admin/instances/traffic-reset", s.requireAdmin(s.csrfGuard(s.handleAdminInstanceTrafficReset)))
	mux.HandleFunc("POST /admin/instances/migrate", s.requireAdmin(s.csrfGuard(s.handleAdminInstanceMigrate)))
	mux.HandleFunc("GET /admin/orders", s.requireAdmin(s.handleAdminOrders))
	mux.HandleFunc("POST /admin/orders/confirm", s.requireAdmin(s.csrfGuard(s.handleAdminOrderConfirm)))
	mux.HandleFunc("GET /admin/settings", s.requireAdmin(s.handleAdminSettings))
	mux.HandleFunc("POST /admin/settings", s.requireAdmin(s.csrfGuard(s.handleAdminSettingsSave)))
	mux.HandleFunc("GET /admin/audit", s.requireAdmin(s.handleAdminAudit))

	return secureHeaders(s.langCookie(s.resolveSession(mux)))
}

// cacheStatic marks embedded assets as long-lived and immutable.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}

// ---------- rendering ----------

// page is the data envelope handed to every template.
type page struct {
	Site   string
	Title  string
	User   *store.User
	CSRF   string
	Active string
	Flash  string
	Error  string
	Data   map[string]any
	Year   int
	Signup bool
	Notice string // site-wide announcement
	Lang   string // "zh" or "en"
}

// newPage builds the common template envelope for a request.
func (s *Server) newPage(r *http.Request, title, active string) *page {
	ctx := r.Context()
	// A stored site name overrides the env default so admins can rename
	// without a restart; likewise the announcement.
	site := s.db.Setting(ctx, "site_name", s.cfg.SiteName)
	p := &page{
		Site:   site,
		Title:  title,
		Active: active,
		Data:   map[string]any{},
		Year:   time.Now().Year(),
		Signup: s.cfg.AllowSignup,
		Notice: s.db.Setting(ctx, "announcement", ""),
		Lang:   langOf(r),
	}
	if ac := userFrom(r); ac != nil {
		p.User = ac.User
		p.CSRF = ac.Session.CSRF
	}
	return p
}

// render writes a template. Templates are cloned per request so the T
// translation function can be bound to the request's language; html/template
// still contextually escapes every interpolation.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, err := s.tmpl.Clone()
	if err == nil {
		t = t.Funcs(template.FuncMap{"T": translator(p.Lang)})
		err = t.ExecuteTemplate(w, name, p)
	}
	if err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// renderError shows a styled error page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.WriteHeader(code)
	p := s.newPage(r, "出错了", "")
	p.Error = msg
	p.Data["Code"] = code
	s.render(w, r, "error.html", p)
}

// jsonOK writes a success envelope.
func (s *Server) jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if v == nil {
		v = map[string]any{"ok": true}
	}
	_ = json.NewEncoder(w).Encode(v)
}

// jsonErr writes an error envelope.
func (s *Server) jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ---------- template helpers ----------

func (s *Server) funcMap() template.FuncMap {
	return template.FuncMap{
		"T": func(zh string) string { return zh },
		"fmtTime": func(ts int64) string {
			if ts <= 0 {
				return "—"
			}
			return time.Unix(ts, 0).Format("2006-01-02 15:04")
		},
		"fmtDate": func(ts int64) string {
			if ts <= 0 {
				return "永久"
			}
			return time.Unix(ts, 0).Format("2006-01-02")
		},
		"since": func(ts int64) string {
			if ts <= 0 {
				return "从未"
			}
			d := time.Since(time.Unix(ts, 0))
			switch {
			case d < time.Minute:
				return "刚刚"
			case d < time.Hour:
				return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d 小时前", int(d.Hours()))
			default:
				return fmt.Sprintf("%d 天前", int(d.Hours()/24))
			}
		},
		"daysLeft": func(ts int64) int {
			if ts <= 0 {
				return -1
			}
			return int(time.Until(time.Unix(ts, 0)).Hours() / 24)
		},
		"bytes":       humanBytes,
		"mb":          func(n int) string { return humanBytes(int64(n) * 1024 * 1024) },
		"gb":          func(n int) string { return fmt.Sprintf("%d GB", n) },
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int64) int64 { return a - b },
		"statusClass": statusClass,
		"statusText":  statusText,
		"modeText": func(m string) string {
			switch m {
			case "ipv6":
				return "NAT + 独立 IPv6"
			case "ipv6only":
				return "仅 IPv6"
			case "ipv4":
				return "NAT + 独立公网 IPv4"
			case "ipv4only":
				return "仅独立公网 IPv4"
			case "ipv4v6":
				return "独立公网 IPv4 + IPv6"
			default:
				return "NAT"
			}
		},
		"payText": func(m string) string {
			switch m {
			case "alipay":
				return "支付宝"
			case "wxpay":
				return "微信支付"
			case "usdt":
				return "USDT"
			default:
				return m
			}
		},
		"orderStatus": func(st string) string {
			switch st {
			case "paid":
				return "已到账"
			case "expired":
				return "已过期"
			default:
				return "待支付"
			}
		},
		"instTypeText": func(t string) string {
			if t == "vm" {
				return "KVM 虚拟机 (Beta)"
			}
			return "容器"
		},
		"trafficModeText": func(m string) string {
			switch m {
			case "up":
				return "仅上行"
			case "down":
				return "仅下行"
			default:
				return "双向"
			}
		},
		"featText": func(f string) string {
			switch f {
			case "tun":
				return "TUN/TAP"
			case "fuse":
				return "FUSE"
			case "privileged":
				return "特权"
			case "nesting":
				return "嵌套"
			default:
				return f
			}
		},
		"osIcon": func(family string) string {
			if strings.HasPrefix(strings.ToLower(family), "alpine") {
				return "alpine"
			}
			return "debian"
		},
		"join":  strings.Join,
		"money": fmtMoney,
		"txKind": func(k string) string {
			switch k {
			case "recharge":
				return "充值"
			case "purchase":
				return "开通扣费"
			case "refund":
				return "退款"
			case "admin":
				return "管理员调整"
			default:
				return k
			}
		},
		"split": func(sep, s string) []string {
			var out []string
			for _, v := range strings.Split(s, sep) {
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, v)
				}
			}
			return out
		},
		"pct": func(a, b int) int {
			if b <= 0 {
				return 0
			}
			v := a * 100 / b
			if v > 100 {
				return 100
			}
			return v
		},
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// statusClass maps an instance status onto a CSS modifier.
func statusClass(s string) string {
	switch strings.ToLower(s) {
	case "running":
		return "ok"
	case "stopped", "frozen":
		return "off"
	case "provisioning", "pending", "migrating":
		return "wait"
	case "expired", "error", "failed", "overquota":
		return "bad"
	default:
		return "off"
	}
}

// statusText localises an instance status.
func statusText(s string) string {
	switch strings.ToLower(s) {
	case "running":
		return "运行中"
	case "stopped":
		return "已停止"
	case "frozen":
		return "已冻结"
	case "provisioning":
		return "创建中"
	case "expired":
		return "已到期"
	case "overquota":
		return "流量超限"
	case "error", "failed":
		return "异常"
	default:
		return s
	}
}

// ---------- small helpers ----------

// pathID reads and validates the {id} path parameter.
func pathID(r *http.Request) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// formInt reads a bounded integer form field, falling back to def.
func formInt(r *http.Request, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
	if err != nil || v < lo || v > hi {
		return def
	}
	return v
}

// formInt64 reads a non-negative int64 form field.
func formInt64(r *http.Request, key string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(key)), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// formStr reads a trimmed, length-capped form field.
func formStr(r *http.Request, key string, max int) string {
	v := strings.TrimSpace(r.FormValue(key))
	if len(v) > max {
		v = v[:max]
	}
	return v
}

func formBool(r *http.Request, key string) bool {
	v := r.FormValue(key)
	return v == "1" || v == "on" || v == "true"
}

func parseURLHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return u.Host, nil
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
