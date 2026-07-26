package panel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cubpanel/internal/store"
)

const (
	sessionCookie = "cubpanel_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// randToken returns n bytes of URL-safe randomness.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashPassword produces a bcrypt hash at a deliberate cost.
func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	return string(b), err
}

// checkPassword verifies a plaintext password against a stored hash.
func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// randomPassword generates a root password from an unambiguous alphabet, so
// it is safe to display, retype, and pass through shell environments.
func randomPassword(n int) string {
	const alpha = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return string(b)
}

// ---------- security headers ----------

// secureHeaders applies a strict baseline policy to every response.
//
// script-src stays locked to 'self' with no unsafe-inline and no unsafe-eval,
// which is what actually stops XSS. style-src permits inline because xterm.js
// injects a <style> element at runtime; inline CSS cannot execute script under
// this policy, so the residual risk is cosmetic.
func secureHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self' ws: wss:; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"object-src 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- sessions ----------

// ctxKey is the private context key type for request-scoped values.
type ctxKey int

const ctxUser ctxKey = iota

// authCtx is what the middleware attaches to an authenticated request.
type authCtx struct {
	User    *store.User
	Session *store.Session
}

// userFrom returns the authenticated principal, if any.
func userFrom(r *http.Request) *authCtx {
	v, _ := r.Context().Value(ctxUser).(*authCtx)
	return v
}

// startSession issues a session and sets the cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	id := randToken(32)
	csrf := randToken(32)
	if err := s.db.CreateSession(r.Context(), id, u.ID, csrf,
		clientIP(r), r.UserAgent(), sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

// clearSession drops the session row and expires the cookie.
func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.db.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// resolveSession attaches the principal to the request context when the
// cookie names a live session belonging to an active account.
func (s *Server) resolveSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, err := s.db.GetSession(r.Context(), c.Value)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.db.UserByID(r.Context(), sess.UserID)
		if err != nil || u.Status != "active" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, &authCtx{User: u, Session: sess})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireUser gates a handler behind authentication.
func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) == nil {
			if wantsJSON(r) {
				s.jsonErr(w, http.StatusUnauthorized, "请先登录")
				return
			}
			http.Redirect(w, r, "/login?next="+safeNext(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// requireAdmin gates a handler behind the admin flag.
func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r).User.IsAdmin {
			if wantsJSON(r) {
				s.jsonErr(w, http.StatusForbidden, "需要管理员权限")
				return
			}
			s.renderError(w, r, http.StatusForbidden, "需要管理员权限")
			return
		}
		h(w, r)
	})
}

// ---------- CSRF ----------

// checkCSRF validates the per-session token on every state-changing request.
// The token travels in a form field or the X-CSRF-Token header.
func (s *Server) checkCSRF(r *http.Request) bool {
	ac := userFrom(r)
	if ac == nil {
		return false
	}
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.FormValue("_csrf")
	}
	return subtle.ConstantTimeCompare([]byte(ac.Session.CSRF), []byte(got)) == 1
}

// csrfGuard wraps a mutating handler with an origin check and token check.
func (s *Server) csrfGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			s.jsonErr(w, http.StatusForbidden, "跨站请求被拒绝")
			return
		}
		if !s.checkCSRF(r) {
			if wantsJSON(r) {
				s.jsonErr(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面重试")
				return
			}
			s.renderError(w, r, http.StatusForbidden, "CSRF 校验失败，请刷新页面重试")
			return
		}
		h(w, r)
	}
}

// sameOrigin compares the Origin/Referer host against the request host. It is
// a second, independent line of defence alongside the CSRF token.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		// No Origin on a same-origin form post from some older clients; the
		// SameSite=Lax cookie and the CSRF token still cover this case.
		return true
	}
	u, err := parseURLHost(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u, r.Host)
}

// ---------- rate limiting ----------

// limiter is a fixed-window counter keyed by caller identity.
type limiter struct {
	mu     sync.Mutex
	hits   map[string]*window
	limit  int
	period time.Duration
}

type window struct {
	count int
	reset time.Time
}

func newLimiter(limit int, period time.Duration) *limiter {
	l := &limiter{hits: map[string]*window{}, limit: limit, period: period}
	go l.reap()
	return l
}

// allow records an attempt, reporting whether it is within budget.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.hits[key]
	if !ok || time.Now().After(w.reset) {
		l.hits[key] = &window{count: 1, reset: time.Now().Add(l.period)}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// reset clears a key, called after a successful authentication.
func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

func (l *limiter) reap() {
	for range time.Tick(5 * time.Minute) {
		l.mu.Lock()
		for k, w := range l.hits {
			if time.Now().After(w.reset) {
				delete(l.hits, k)
			}
		}
		l.mu.Unlock()
	}
}

// rateLimit rejects a request that exceeds the given limiter.
func (s *Server) rateLimit(l *limiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			if wantsJSON(r) {
				s.jsonErr(w, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
				return
			}
			s.renderError(w, r, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
			return
		}
		h(w, r)
	}
}

// ---------- input validation ----------

var (
	emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,24}$`)
	// codeRe matches the format the panel itself generates.
	codeRe  = regexp.MustCompile(`^[A-Z0-9]{4}(-[A-Z0-9]{4}){3}$`)
	labelRe = regexp.MustCompile(`^[\p{L}\p{N} _.\-]{0,32}$`)
	slugRe  = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// ipv4AddrRe loosely matches a dotted-quad address.
	ipv4AddrRe = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)
	// imageFpRe matches an image fingerprint (or unambiguous prefix).
	imageFpRe = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	// imageAliasRe mirrors the agent's own check, so the admin form cannot
	// store an alias the node will later reject.
	imageAliasRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{1,63}$`)
)

func validEmail(s string) bool {
	return len(s) <= 254 && emailRe.MatchString(s)
}

// validPassword enforces a usable minimum without being obstructive.
func validPassword(s string) bool {
	return len(s) >= 8 && len(s) <= 128
}

// ---------- misc helpers ----------

// clientIP resolves the caller address, honouring a trusted proxy header only
// when the operator has opted in.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" && trustProxy {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" && trustProxy {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// trustProxy is set at startup from configuration.
var trustProxy bool

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") ||
		r.Header.Get("X-Requested-With") == "fetch"
}

// safeNext sanitises a post-login redirect so it can only stay on this site.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return "%2F"
	}
	return urlQueryEscape(v)
}
