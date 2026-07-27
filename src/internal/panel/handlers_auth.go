package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"cubpanel/internal/store"
)

const (
	verifyCodeTTL     = 15 * time.Minute
	verifyMaxAttempts = 8
)

// handleHome renders the public storefront.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.newPage(r, "首页", "home")

	plans, err := s.db.ListPlans(ctx, true)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "读取套餐失败")
		return
	}
	nodes, _ := s.db.ListNodes(ctx, true)

	// The storefront exposes node names and regions only — never endpoints or
	// secrets.
	type publicNode struct {
		Name, Region string
		Used, Cap    int
		Online       bool
	}
	pub := make([]publicNode, 0, len(nodes))
	for _, n := range nodes {
		pub = append(pub, publicNode{
			Name: n.Name, Region: n.Region,
			Used: n.InstanceCount, Cap: n.MaxInstances,
			Online: n.LastStatus == "ok",
		})
	}

	p.Data["Plans"] = plans
	p.Data["Nodes"] = pub
	s.render(w, r, "home.html", p)
}

// handleLoginPage renders the sign-in form.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/app", http.StatusSeeOther)
		return
	}
	p := s.newPage(r, "登录", "login")
	p.Data["Next"] = sanitizeNext(r.URL.Query().Get("next"))
	s.render(w, r, "login.html", p)
}

// handleLoginPost authenticates a sign-in attempt.
func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := strings.ToLower(formStr(r, "email", 254))
	password := r.FormValue("password")
	next := sanitizeNext(r.FormValue("next"))

	fail := func(msg string) {
		p := s.newPage(r, "登录", "login")
		p.Error = msg
		p.Data["Email"] = email
		p.Data["Next"] = next
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", p)
	}

	u, err := s.db.UserByEmail(ctx, email)
	if err != nil {
		// Run a hash comparison anyway so a missing account and a wrong
		// password take indistinguishable time.
		checkPassword("$2a$12$0000000000000000000000000000000000000000000000000000u", password)
		fail("邮箱或密码错误")
		return
	}
	if !checkPassword(u.PasswordHash, password) {
		s.db.Audit(ctx, u.ID, email, "login.fail", "密码错误", clientIP(r))
		fail("邮箱或密码错误")
		return
	}
	if u.Status != "active" {
		fail("账号已被停用，请联系管理员")
		return
	}

	if err := s.startSession(w, r, u); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "创建会话失败")
		return
	}
	s.limits.login.reset(clientIP(r))
	_ = s.db.TouchLogin(ctx, u.ID, clientIP(r))
	s.db.Audit(ctx, u.ID, email, "login.ok", "", clientIP(r))

	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleRegisterPage renders the sign-up form.
func (s *Server) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowSignup {
		s.renderError(w, r, http.StatusForbidden, "本站未开放注册")
		return
	}
	if userFrom(r) != nil {
		http.Redirect(w, r, "/app", http.StatusSeeOther)
		return
	}
	p := s.newPage(r, "注册", "register")
	p.Data["MailVerify"] = s.mailVerifyEnabled(r.Context())
	s.render(w, r, "register.html", p)
}

// handleRegisterPost creates an account, or starts email verification when
// that feature is enabled in site settings.
func (s *Server) handleRegisterPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowSignup {
		s.renderError(w, r, http.StatusForbidden, "本站未开放注册")
		return
	}
	ctx := r.Context()
	email := strings.ToLower(formStr(r, "email", 254))
	pw := r.FormValue("password")
	pw2 := r.FormValue("password2")

	fail := func(msg string) {
		p := s.newPage(r, "注册", "register")
		p.Error = msg
		p.Data["Email"] = email
		p.Data["MailVerify"] = s.mailVerifyEnabled(ctx)
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "register.html", p)
	}

	switch {
	case !validEmail(email):
		fail("请输入有效的邮箱地址")
		return
	case !validPassword(pw):
		fail("密码长度需为 8–128 位")
		return
	case pw != pw2:
		fail("两次输入的密码不一致")
		return
	}

	if _, err := s.db.UserByEmail(ctx, email); err == nil {
		fail("该邮箱已被注册")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		fail("注册失败，请稍后重试")
		return
	}

	hash, err := hashPassword(pw)
	if err != nil {
		fail("注册失败，请稍后重试")
		return
	}

	// Optional email verification: hold the password hash + send a 6-digit
	// code instead of creating the user immediately.
	if s.mailVerifyEnabled(ctx) {
		if _, ok := s.loadSMTP(ctx); !ok {
			fail("站点已开启邮箱验证，但管理员尚未配置 SMTP，请稍后再试或联系管理员")
			return
		}
		code, err := randomDigits(6)
		if err != nil {
			fail("注册失败，请稍后重试")
			return
		}
		token := randToken(24)
		if err := s.db.SaveEmailVerification(ctx, email, hash, code, token, verifyCodeTTL); err != nil {
			fail("注册失败，请稍后重试")
			return
		}
		site := s.db.Setting(ctx, "site_name", s.cfg.SiteName)
		if err := s.sendVerifyCode(ctx, email, code, site); err != nil {
			// Keep the pending row so a later resend (or re-register) can reuse the flow.
			fail("验证码发送失败：" + err.Error() + "。请检查邮箱或稍后重试。")
			return
		}
		s.db.Audit(ctx, 0, email, "user.register.verify_sent", "", clientIP(r))
		http.Redirect(w, r, "/register/verify?token="+urlQueryEscape(token), http.StatusSeeOther)
		return
	}

	// Direct registration (verification off).
	if err := s.finishRegistration(w, r, email, hash); err != nil {
		fail(err.Error())
		return
	}
}

// finishRegistration creates the user, starts a session and redirects to /app.
func (s *Server) finishRegistration(w http.ResponseWriter, r *http.Request, email, hash string) error {
	ctx := r.Context()
	// The very first account bootstraps the administrator.
	count, _ := s.db.CountUsers(ctx)
	uid, err := s.db.CreateUser(ctx, email, hash, count == 0)
	if err != nil {
		return errors.New("该邮箱已被注册")
	}
	u, err := s.db.UserByID(ctx, uid)
	if err != nil {
		return errors.New("注册失败，请稍后重试")
	}
	if err := s.startSession(w, r, u); err != nil {
		return errors.New("创建会话失败")
	}
	s.db.Audit(ctx, uid, email, "user.register", "", clientIP(r))
	http.Redirect(w, r, "/app", http.StatusSeeOther)
	return nil
}

// handleRegisterVerifyPage shows the 6-digit code form.
func (s *Server) handleRegisterVerifyPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowSignup {
		s.renderError(w, r, http.StatusForbidden, "本站未开放注册")
		return
	}
	if userFrom(r) != nil {
		http.Redirect(w, r, "/app", http.StatusSeeOther)
		return
	}
	token := formStr(r, "token", 128)
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	p := s.newPage(r, "邮箱验证", "register")
	p.Data["Token"] = token
	if token == "" {
		p.Error = "缺少验证令牌，请重新注册"
		s.render(w, r, "register_verify.html", p)
		return
	}
	v, err := s.db.EmailVerificationByToken(r.Context(), token)
	if err != nil || time.Now().Unix() > v.ExpiresAt {
		p.Error = "验证已过期或无效，请重新注册"
		p.Data["Token"] = ""
		s.render(w, r, "register_verify.html", p)
		return
	}
	p.Data["Email"] = maskEmail(v.Email)
	if r.URL.Query().Get("resent") == "1" {
		p.Flash = "验证码已重新发送，请查收邮箱"
	}
	if r.URL.Query().Get("err") == "send" {
		p.Error = "验证码发送失败，请稍后重试或检查邮箱地址"
	}
	s.render(w, r, "register_verify.html", p)
}

// handleRegisterVerifyPost checks the 6-digit code and completes registration.
func (s *Server) handleRegisterVerifyPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowSignup {
		s.renderError(w, r, http.StatusForbidden, "本站未开放注册")
		return
	}
	ctx := r.Context()
	token := formStr(r, "token", 128)
	code := strings.TrimSpace(formStr(r, "code", 8))

	fail := func(msg, emailMask string) {
		p := s.newPage(r, "邮箱验证", "register")
		p.Error = msg
		p.Data["Token"] = token
		p.Data["Email"] = emailMask
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "register_verify.html", p)
	}

	if token == "" || len(code) != 6 {
		fail("请输入 6 位验证码", "")
		return
	}
	v, err := s.db.EmailVerificationByToken(ctx, token)
	if err != nil {
		fail("验证已过期或无效，请重新注册", "")
		return
	}
	if time.Now().Unix() > v.ExpiresAt {
		_ = s.db.DeleteEmailVerification(ctx, v.ID)
		fail("验证码已过期，请重新注册", maskEmail(v.Email))
		return
	}
	if v.Attempts >= verifyMaxAttempts {
		_ = s.db.DeleteEmailVerification(ctx, v.ID)
		fail("验证失败次数过多，请重新注册", maskEmail(v.Email))
		return
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(v.Code)) != 1 {
		_ = s.db.BumpEmailVerificationAttempts(ctx, v.ID)
		fail("验证码错误，请重试", maskEmail(v.Email))
		return
	}

	// Race-safe: if someone else registered the same email in the meantime.
	if _, err := s.db.UserByEmail(ctx, v.Email); err == nil {
		_ = s.db.DeleteEmailVerification(ctx, v.ID)
		fail("该邮箱已被注册", maskEmail(v.Email))
		return
	}

	count, _ := s.db.CountUsers(ctx)
	uid, err := s.db.CreateUser(ctx, v.Email, v.PasswordHash, count == 0)
	if err != nil {
		fail("该邮箱已被注册", maskEmail(v.Email))
		return
	}
	_ = s.db.DeleteEmailVerification(ctx, v.ID)
	u, err := s.db.UserByID(ctx, uid)
	if err != nil {
		fail("注册失败，请稍后重试", maskEmail(v.Email))
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "创建会话失败")
		return
	}
	s.db.Audit(ctx, uid, v.Email, "user.register", "email_verified", clientIP(r))
	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

// handleRegisterVerifyResend re-issues a 6-digit code for an existing challenge.
func (s *Server) handleRegisterVerifyResend(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowSignup {
		s.renderError(w, r, http.StatusForbidden, "本站未开放注册")
		return
	}
	ctx := r.Context()
	token := formStr(r, "token", 128)
	if token == "" {
		s.renderError(w, r, http.StatusBadRequest, "缺少验证令牌")
		return
	}
	v, err := s.db.EmailVerificationByToken(ctx, token)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "验证已过期或无效，请重新注册")
		return
	}
	// Allow resend even after expiry of the old code — issue a fresh one with
	// the same password hash and a new token so the form URL stays valid.
	code, err := randomDigits(6)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "生成验证码失败")
		return
	}
	newToken := randToken(24)
	if err := s.db.SaveEmailVerification(ctx, v.Email, v.PasswordHash, code, newToken, verifyCodeTTL); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "保存验证码失败")
		return
	}
	site := s.db.Setting(ctx, "site_name", s.cfg.SiteName)
	if err := s.sendVerifyCode(ctx, v.Email, code, site); err != nil {
		// Keep the new pending row so the user can try again.
		http.Redirect(w, r, "/register/verify?token="+urlQueryEscape(newToken)+"&err=send", http.StatusSeeOther)
		return
	}
	s.db.Audit(ctx, 0, v.Email, "user.register.verify_resent", "", clientIP(r))
	http.Redirect(w, r, "/register/verify?token="+urlQueryEscape(newToken)+"&resent=1", http.StatusSeeOther)
}

// randomDigits returns an n-digit decimal code (leading zeros allowed).
func randomDigits(n int) (string, error) {
	if n <= 0 || n > 12 {
		return "", fmt.Errorf("invalid digit length")
	}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + v.Int64()))
	}
	return b.String(), nil
}

// maskEmail hides the local-part middle so templates can show where the code went.
func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 1 {
		return email
	}
	local, domain := email[:at], email[at:]
	if len(local) <= 2 {
		return local[:1] + "***" + domain
	}
	return local[:1] + "***" + local[len(local)-1:] + domain
}

// handleLogout ends the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ac := userFrom(r); ac != nil {
		s.db.Audit(r.Context(), ac.User.ID, ac.User.Email, "logout", "", clientIP(r))
	}
	s.clearSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleAccountPage renders account settings.
func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "账号设置", "settings")
	n, _ := s.db.CountUserInstances(r.Context(), p.User.ID)
	p.Data["InstanceCount"] = n
	s.render(w, r, "account.html", p)
}

// handleAccountPassword changes the signed-in user's password.
func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ac := userFrom(r)
	cur := r.FormValue("current")
	pw := r.FormValue("password")
	pw2 := r.FormValue("password2")

	render := func(errMsg, flash string) {
		p := s.newPage(r, "账号设置", "settings")
		p.Error, p.Flash = errMsg, flash
		n, _ := s.db.CountUserInstances(ctx, ac.User.ID)
		p.Data["InstanceCount"] = n
		s.render(w, r, "account.html", p)
	}

	switch {
	case !checkPassword(ac.User.PasswordHash, cur):
		render("当前密码不正确", "")
		return
	case !validPassword(pw):
		render("新密码长度需为 8–128 位", "")
		return
	case pw != pw2:
		render("两次输入的新密码不一致", "")
		return
	}

	hash, err := hashPassword(pw)
	if err != nil {
		render("修改失败，请稍后重试", "")
		return
	}
	if err := s.db.SetUserPassword(ctx, ac.User.ID, hash); err != nil {
		render("修改失败，请稍后重试", "")
		return
	}
	// Invalidate every other session, then re-issue one for this browser.
	_ = s.db.DeleteUserSessions(ctx, ac.User.ID)
	if err := s.startSession(w, r, ac.User); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.db.Audit(ctx, ac.User.ID, ac.User.Email, "user.password", "", clientIP(r))
	http.Redirect(w, r, "/app/settings?ok=1", http.StatusSeeOther)
}

// sanitizeNext keeps post-login redirects on this origin.
func sanitizeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") ||
		strings.Contains(v, "\\") || strings.ContainsAny(v, "\r\n") {
		return "/app"
	}
	return v
}
