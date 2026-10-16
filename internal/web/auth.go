package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"sort"
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
				s.csrfFailed(w, r)
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

// csrfFailed answers a POST whose token did not match.
//
// It used to redirect 303 to the same path with a flash. That reads well and
// works for the handful of routes with a GET form at the same address — but
// most mutating routes are POST-only, and a 303 turns the request into a GET
// the mux has no handler for. The operator got a bare "405 Method Not Allowed"
// and the friendly explanation was discarded with the response.
//
// A rendered 403 says the same thing on every route.
func (s *Server) csrfFailed(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("rejected a POST with a bad or missing CSRF token",
		"path", r.URL.Path, "ip", s.clientIP(r), "user", currentUserEmail(r))

	w.WriteHeader(http.StatusForbidden)
	s.render(w, r, "error", pageData{
		Title: "That form had expired",
		Data: map[string]any{
			"Heading": "That form had expired",
			"Message": "This usually means the page was left open for a long time, or you signed in " +
				"again in another tab. Nothing was changed. Go back, reload the page, and try again.",
			"Back": backLink(r),
		},
	})
}

// backLink is somewhere sensible to send someone after an error, preferring
// where they came from when that was this site.
func backLink(r *http.Request) string {
	if referer := r.Referer(); referer != "" {
		if path, err := parseSameSite(referer, r); err == nil && path != "" {
			return path
		}
	}
	return "/"
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
		target += "?next=" + url.QueryEscape(r.URL.RequestURI())
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

// loginThrottle slows down repeated failed sign-ins.
//
// Two things are tracked, and the second is the one that matters. Per-IP is the
// obvious key and the weak one: the address comes from a header when feather
// sits behind a proxy, so an attacker who can set it gets a fresh bucket per
// request. Per-account cannot be sidestepped that way — whoever is guessing has
// to name the account they are guessing at.
//
// Both maps are bounded. Unbounded, a spoofed address per request grew them
// without limit, inside the same process that runs the archives.
var loginThrottle = newThrottle()

const (
	throttleAfter    = 5
	throttleCooldown = 5 * time.Minute
	// throttleMaxKeys bounds memory. Reaching it means either a very busy panel
	// or someone cycling keys; evicting the oldest entries is right in both
	// cases, since the entries being evicted are the least recently active.
	throttleMaxKeys = 4096
)

type throttle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	failures int
	until    time.Time
	seen     time.Time
}

func newThrottle() *throttle {
	return &throttle{entries: map[string]*throttleEntry{}}
}

// blocked reports how long this key must wait, if at all.
func (t *throttle) blocked(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.entries[key]
	if !ok || time.Now().After(entry.until) {
		return 0, false
	}
	return time.Until(entry.until), true
}

// fail records one failed attempt and starts a cooldown once there have been
// enough of them.
func (t *throttle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.evictLocked()
	entry, ok := t.entries[key]
	if !ok {
		entry = &throttleEntry{}
		t.entries[key] = entry
	}
	entry.seen = time.Now()
	entry.failures++
	if entry.failures >= throttleAfter {
		entry.until = time.Now().Add(throttleCooldown)
		entry.failures = 0
	}
}

// succeed clears a key after a successful sign-in.
func (t *throttle) succeed(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// evictLocked drops expired entries, and if that is not enough, the least
// recently seen ones. The caller holds the lock.
func (t *throttle) evictLocked() {
	if len(t.entries) < throttleMaxKeys {
		return
	}

	now := time.Now()
	for key, entry := range t.entries {
		// An entry with no live cooldown and no recent activity is finished
		// with; the cooldown window is how long it could still matter.
		if now.After(entry.until) && now.Sub(entry.seen) > throttleCooldown {
			delete(t.entries, key)
		}
	}
	if len(t.entries) < throttleMaxKeys {
		return
	}

	// Still full: drop the oldest half rather than scanning on every failure.
	oldest := make([]string, 0, len(t.entries))
	for key := range t.entries {
		oldest = append(oldest, key)
	}
	sort.Slice(oldest, func(i, j int) bool {
		return t.entries[oldest[i]].seen.Before(t.entries[oldest[j]].seen)
	})
	for _, key := range oldest[:len(oldest)/2] {
		delete(t.entries, key)
	}
}

// throttleKeys are the two buckets one sign-in attempt counts against.
func throttleKeys(ip, email string) (ipKey, accountKey string) {
	return "ip:" + ip, "account:" + strings.ToLower(strings.TrimSpace(email))
}

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
	ip := s.clientIP(r)

	ipKey, accountKey := throttleKeys(ip, email)
	for _, key := range []string{ipKey, accountKey} {
		wait, blocked := loginThrottle.blocked(key)
		if !blocked {
			continue
		}
		// The same message either way. Saying "this account is locked" would
		// confirm the account exists, which is the one thing the login page is
		// careful not to reveal anywhere else.
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
		loginThrottle.fail(ipKey)
		loginThrottle.fail(accountKey)
		s.log.Warn("failed login", "email", email, "ip", ip, "ip_trusted", s.trustsProxy(r))
		s.render(w, r, "login", pageData{
			Title: "Sign in",
			Nav:   "login",
			Flash: &flash{Kind: "error", Message: err.Error()},
			Data:  map[string]any{"Next": next, "Email": email},
		})
		return
	}

	loginThrottle.succeed(ipKey)
	loginThrottle.succeed(accountKey)
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
	token, session, err := s.app.Store.CreateSession(r.Context(), user.ID, r.UserAgent(), s.clientIP(r))
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

// safeNext keeps the post-login redirect on this site: an open redirect here
// would turn the login page into a phishing hop.
//
// The prefix checks it used to do were not enough. "/\evil.com" starts with "/"
// and not "//", so it passed — and browsers normalise the backslash to a slash,
// making it the protocol-relative "//evil.com" the check was meant to catch.
// Parsing and requiring the result to name no scheme and no host is the check
// that actually holds, because it asks the question directly.
func safeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return ""
	}
	// url.Parse is permissive about backslashes; browsers are not, so reject
	// them rather than reasoning about who normalises what.
	if strings.ContainsAny(raw, `\`) {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" {
		return ""
	}
	// Both forms have to look like a path on this site. Browsers do not decode
	// %2F before resolving a Location, so "/%2F%2Fevil.com" is same-origin to
	// them — but some proxies normalise it on the way through, and then it is
	// "//evil.com". Checking the decoded form too costs nothing and removes the
	// need to be right about which intermediary does what.
	for _, path := range []string{parsed.EscapedPath(), parsed.Path} {
		if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
			return ""
		}
	}
	return parsed.RequestURI()
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
	// A same-host referer can still carry a path such as "//evil.com" or
	// "/\evil.com", which a browser resolves off-site once it is a Location
	// or an href. safeNext already knows every such shape.
	path := safeNext(parsed.RequestURI())
	if path == "" {
		return "", errors.New("referer path would leave the site")
	}
	return path, nil
}
