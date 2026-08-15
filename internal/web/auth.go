package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/unifers/feather/internal/store"
)

const (
	sessionCookie = "feather_session"
	csrfCookie    = "feather_csrf"
	csrfField     = "csrf"
)

type contextKey string

const sessionContextKey contextKey = "feather.session"

type sessionContext struct {
	session store.Session
	user    store.AdminUser
}

// guard wraps a handler so it only runs for a logged-in administrator, and so
// every state-changing request carries a valid CSRF token.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			s.redirectToLogin(w, r)
			return
		}

		session, user, err := s.app.Store.SessionByToken(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.clearSessionCookie(w)
				s.redirectToLogin(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}

		if r.Method == http.MethodPost {
			if !validCSRF(r, session.CSRFToken) {
				// Almost always a stale tab rather than an attack, so say so
				// plainly instead of showing a bare 403.
				s.setFlash(w, "error", "That form had expired. Please try again.")
				http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
				return
			}
		}

		// Someone actively using the panel should not be logged out mid-edit.
		if time.Until(session.ExpiresAt) < store.SessionTTL/2 {
			if err := s.app.Store.TouchSession(r.Context(), cookie.Value); err != nil {
				s.log.Warn("could not extend session", "error", err)
			}
		}

		ctx := context.WithValue(r.Context(), sessionContextKey, sessionContext{session: session, user: user})
		next(w, r.WithContext(ctx))
	}
}

func sessionFrom(r *http.Request) (store.Session, store.AdminUser, bool) {
	value, ok := r.Context().Value(sessionContextKey).(sessionContext)
	if !ok {
		return store.Session{}, store.AdminUser{}, false
	}
	return value.session, value.user, true
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	// Bounce back to where they were headed, but only within this site.
	if r.Method == http.MethodGet && r.URL.Path != "/" {
		target += "?next=" + urlEscape(r.URL.RequestURI())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// --- first-run setup -------------------------------------------------------

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "setup", pageData{
		Title: "Set up feather",
		Nav:   "setup",
		Data: map[string]any{
			"MinPassword": store.MinPasswordLength,
			"Email":       "",
		},
	})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !validPreSessionCSRF(r) {
		s.setFlash(w, "error", "That form had expired. Please try again.")
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	fail := func(message string) {
		s.render(w, r, "setup", pageData{
			Title: "Set up feather",
			Nav:   "setup",
			Flash: &flash{Kind: "error", Message: message},
			Data:  map[string]any{"MinPassword": store.MinPasswordLength, "Email": email},
		})
	}

	if password != confirm {
		fail("The two passwords do not match.")
		return
	}

	// Racing two setup submissions would otherwise create two administrators.
	needsSetup, err := s.app.Store.NeedsSetup(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !needsSetup {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.app.Store.CreateAdmin(r.Context(), email, password)
	if err != nil {
		fail(err.Error())
		return
	}
	s.log.Info("administrator created", "email", user.Email)

	if err := s.startSession(w, r, user); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setFlash(w, "ok", "Welcome. Add a MongoDB account to get started.")
	http.Redirect(w, r, "/mongo/new", http.StatusSeeOther)
}

// --- login and logout ------------------------------------------------------

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// Already signed in — no reason to show the form again.
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if _, _, err := s.app.Store.SessionByToken(r.Context(), cookie.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.render(w, r, "login", pageData{
		Title: "Sign in",
		Nav:   "login",
		Data:  map[string]any{"Next": safeNext(r.URL.Query().Get("next"))},
	})
}

// loginThrottle slows down repeated failures from one address. It is a
// deterrent, not a defence — feather binds to localhost by default and expects
// a reverse proxy in front of it.
var loginThrottle = struct {
	sync.Mutex
	failures map[string]int
	until    map[string]time.Time
}{failures: map[string]int{}, until: map[string]time.Time{}}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !validPreSessionCSRF(r) {
		s.setFlash(w, "error", "That form had expired. Please try again.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	ip := clientIP(r)

	if wait, blocked := throttleCheck(ip); blocked {
		s.render(w, r, "login", pageData{
			Title: "Sign in",
			Nav:   "login",
			Flash: &flash{Kind: "error", Message: "Too many failed attempts. Try again in " + humanDuration(wait) + "."},
			Data:  map[string]any{"Next": next, "Email": email},
		})
		return
	}

	user, err := s.app.Store.Authenticate(r.Context(), email, password)
	if err != nil {
		if !errors.Is(err, store.ErrBadCredentials) {
			s.serverError(w, r, err)
			return
		}
		throttleFailure(ip)
		s.log.Warn("failed login", "email", email, "ip", ip)
		s.render(w, r, "login", pageData{
			Title: "Sign in",
			Nav:   "login",
			Flash: &flash{Kind: "error", Message: err.Error()},
			Data:  map[string]any{"Next": next, "Email": email},
		})
		return
	}

	throttleReset(ip)
	if err := s.startSession(w, r, user); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("signed in", "email", user.Email, "ip", ip)

	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := s.app.Store.DeleteSession(r.Context(), cookie.Value); err != nil {
			s.log.Warn("could not delete session", "error", err)
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user store.AdminUser) error {
	token, session, err := s.app.Store.CreateSession(r.Context(), user.ID, r.UserAgent(), clientIP(r))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  session.ExpiresAt,
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// --- CSRF ------------------------------------------------------------------

// validCSRF compares the token in the form against the one bound to the
// session, in constant time.
func validCSRF(r *http.Request, want string) bool {
	got := r.PostFormValue(csrfField)
	if got == "" {
		got = r.Header.Get("X-CSRF-Token")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// issuePreSessionCSRF covers the two forms that exist before a session does:
// setup and login. The token is stored in a cookie and echoed in the form, so
// only a page served by feather itself can post them.
func (s *Server) issuePreSessionCSRF(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   3600,
	})
	return token
}

func validPreSessionCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	return validCSRF(r, cookie.Value)
}

// --- login throttling ------------------------------------------------------

const (
	throttleAfter    = 5
	throttleCooldown = 5 * time.Minute
)

func throttleCheck(ip string) (time.Duration, bool) {
	loginThrottle.Lock()
	defer loginThrottle.Unlock()

	until, ok := loginThrottle.until[ip]
	if !ok || time.Now().After(until) {
		return 0, false
	}
	return time.Until(until), true
}

func throttleFailure(ip string) {
	loginThrottle.Lock()
	defer loginThrottle.Unlock()

	loginThrottle.failures[ip]++
	if loginThrottle.failures[ip] >= throttleAfter {
		loginThrottle.until[ip] = time.Now().Add(throttleCooldown)
		loginThrottle.failures[ip] = 0
	}
}

func throttleReset(ip string) {
	loginThrottle.Lock()
	defer loginThrottle.Unlock()

	delete(loginThrottle.failures, ip)
	delete(loginThrottle.until, ip)
}

// safeNext keeps the post-login redirect on this site: an open redirect here
// would turn the login page into a phishing hop.
func safeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	return raw
}

func urlEscape(raw string) string {
	return strings.ReplaceAll(strings.ReplaceAll(raw, "&", "%26"), "?", "%3F")
}

// parseSameSite accepts a referer only if it points back at this installation,
// so "return where you came from" cannot be steered off-site.
func parseSameSite(referer string, r *http.Request) (string, error) {
	parsed, err := url.Parse(referer)
	if err != nil {
		return "", err
	}
	if parsed.Host != "" && parsed.Host != r.Host {
		return "", errors.New("referer points to another host")
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		return "", errors.New("referer is not a path on this site")
	}
	return parsed.RequestURI(), nil
}
