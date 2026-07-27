package panel

import (
	"net/http"
	"time"
)

// Language handling. The UI ships Chinese source strings; when the visitor
// picks English, T() looks the string up in enDict and falls back to the
// Chinese source if a phrase is not yet translated (so nothing ever renders
// blank). The choice rides on a `lang` cookie, toggled via ?lang=.

// langOf resolves the active language: ?lang= query wins (and gets persisted
// by langCookie), else the cookie, else Chinese.
func langOf(r *http.Request) string {
	if v := r.URL.Query().Get("lang"); v == "en" || v == "zh" {
		return v
	}
	if c, err := r.Cookie("lang"); err == nil && (c.Value == "en" || c.Value == "zh") {
		return c.Value
	}
	return "zh"
}

// langCookie persists a ?lang= choice so it survives navigation.
func (s *Server) langCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("lang"); v == "en" || v == "zh" {
			http.SetCookie(w, &http.Cookie{
				Name: "lang", Value: v, Path: "/",
				Expires: time.Now().AddDate(1, 0, 0), SameSite: http.SameSiteLaxMode,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// translator returns the T func bound to a language.
func translator(lang string) func(string) string {
	if lang != "en" {
		return func(zh string) string { return zh } // Chinese: identity
	}
	return func(zh string) string {
		if en, ok := enDict[zh]; ok {
			return en
		}
		return zh
	}
}

// enDict maps Chinese source strings to English. Extend freely; missing keys
// fall back to the Chinese text.
var enDict = map[string]string{
	// nav / common
	"首页": "Home", "我的实例": "My Instances", "创建实例": "Create Instance",
	"充值": "Top Up", "管理后台": "Admin", "账号设置": "Account", "退出": "Log out",
	"登录": "Log in", "注册": "Sign up", "切换主题": "Toggle theme", "切换语言": "Language",
	"返回实例列表": "Back to instances", "保存": "Save", "取消": "Cancel", "确定": "OK",
	"当前余额": "Balance", "余额": "Balance", "元": "CNY", "去充值": "Top up",

	// home
	"开源的 Incus 容器与虚拟机面板": "Open-source panel for Incus containers & VMs",
	"自助创建、计费与管理":         "Self-service provisioning, billing and management",
	"一套轻量的自助面板，用来创建、售卖与管理基于 Incus 的容器和 KVM 虚拟机。开源、自托管、无外部依赖。": "A lightweight self-hosted panel to create, sell and manage Incus containers and KVM virtual machines. Open-source, no external dependencies.",
	"免费注册": "Sign up", "登录控制台": "Log in", "我的实例 ": "My Instances",
	"秒级交付": "Instant delivery",
	"镜像本地缓存，兑换或下单后自动创建、启动并配置网络。": "Images are cached locally; instances are created, started and networked automatically.",
	"灵活网络": "Flexible networking",
	"NAT 端口转发、独立 IPv6、仅 IPv6，按套餐选择，重启不丢。": "NAT port forwarding, dedicated IPv6 or IPv6-only per plan, persistent across reboots.",
	"资源可控": "Full control",
	"CPU、内存、硬盘、流量与带宽逐项限制，支持弹性升级。": "Per-instance CPU, memory, disk, traffic and bandwidth limits with elastic upgrades.",
	"网页终端": "Web console",
	"内置串行控制台，忘记密码或网络异常也能直接进入系统。": "Built-in serial console to reach the guest even without network access.",
	"可选套餐": "Plans", "查看创建": "View & create", "节点状态": "Node status",
	"实时容量与在线情况。": "Live capacity and availability.", "在线": "Online", "离线": "Offline",
	"类型": "Type", "内存": "RAM", "硬盘": "Disk", "网络": "Network", "时长": "Term",
	"流量": "Traffic", "带宽": "Bandwidth", "价格": "Price", "特性": "Features",
	"永久": "Unlimited", "不限": "Unlimited", "容器": "Container", "天": "days", "基于 Incus 容器技术": "Powered by Incus", "开源项目": "Open source",

	// auth
	"邮箱": "Email", "密码": "Password", "确认密码": "Confirm password",
	"记住我": "Remember me", "还没有账号？": "No account yet?",
	"已有账号？": "Already registered?", "创建账号": "Create account",
	"邮箱验证": "Email verification", "验证码": "Verification code",
	"发送验证码": "Send code", "验证并注册": "Verify & sign up",
	"重新发送验证码": "Resend code",
	"注册需验证邮箱：提交后将向邮箱发送 6 位验证码。": "Email verification is required: a 6-digit code will be sent after submit.",
	"验证码已发送至": "Code sent to", "请输入邮件中的 6 位验证码完成注册。": "Enter the 6-digit code from the email to finish sign-up.",
	"请输入 6 位数字": "6-digit code",

	// recharge
	"充值金额（元）": "Amount (CNY)", "支付方式": "Payment method", "去支付": "Pay",
	"支付宝": "Alipay", "微信支付": "WeChat Pay", "最近充值订单": "Recent orders",
	"订单号": "Order", "金额": "Amount", "方式": "Method", "状态": "Status", "时间": "Time",
	"已到账": "Paid", "待支付": "Pending", "已过期": "Expired",

	// dashboard / instance
	"运行中": "Running", "已停止": "Stopped", "已冻结": "Frozen", "创建中": "Provisioning",
	"已到期": "Expired", "流量超限": "Over quota", "迁移中": "Migrating", "异常": "Error",
	"启动": "Start", "停止": "Stop", "重启": "Restart", "详情": "Details",
	"备注名": "Label", "快照": "Snapshots", "升级配置": "Upgrade", "连接方式": "Connection",
	"运行状态": "Live status", "规格": "Specs", "系统": "OS", "节点": "Node",
	"创建时间": "Created", "到期时间": "Expires", "网络模式": "Network mode",
	"重置 root 密码": "Reset root password",
}
