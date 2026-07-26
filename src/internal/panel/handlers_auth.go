package panel

import (
	"errors"
	"net/http"
	"strings"

	"cubpanel/internal/store"
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
	s.render(w, r, "register.html", s.newPage(r, "注册", "register"))
}

// handleRegisterPost creates an account.
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
	// The very first account bootstraps the administrator.
	count, _ := s.db.CountUsers(ctx)
	uid, err := s.db.CreateUser(ctx, email, hash, count == 0)
	if err != nil {
		fail("该邮箱已被注册")
		return
	}
	u, err := s.db.UserByID(ctx, uid)
	if err != nil {
		fail("注册失败，请稍后重试")
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "创建会话失败")
		return
	}
	s.db.Audit(ctx, uid, email, "user.register", "", clientIP(r))
	http.Redirect(w, r, "/app", http.StatusSeeOther)
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
