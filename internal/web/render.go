package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

//go:embed templates
var templateFS embed.FS

// templateSet holds one fully parsed template per page. Every page defines a
// block called "content", so they cannot share a single set.
type templateSet struct {
	pages map[string]*template.Template
}

func loadTemplates() (*templateSet, error) {
	pages, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}

	set := &templateSet{pages: make(map[string]*template.Template, len(pages))}
	for _, page := range pages {
		parsed, err := template.New("layout.html").Funcs(templateFuncs).
			ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html", page)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", page, err)
		}
		set.pages[strings.TrimSuffix(path.Base(page), ".html")] = parsed
	}
	if len(set.pages) == 0 {
		return nil, fmt.Errorf("no page templates were embedded")
	}
	return set, nil
}

// pageData is what every template receives.
type pageData struct {
	Title    string
	Nav      string
	User     store.AdminUser
	LoggedIn bool
	CSRF     string
	Flash    *flash
	Now      time.Time
	// Refresh, in seconds, makes the page reload itself. Set only while there
	// is something changing to watch — a run in flight — so that pages sitting
	// open do not poll the database forever.
	Refresh int
	Data    map[string]any
}

type flash struct {
	Kind    string // "ok" | "error" | "info"
	Message string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data pageData) {
	tmpl, ok := s.templates.pages[page]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no template named %q", page))
		return
	}

	data.Now = time.Now()
	// A handler that rendered its own message keeps it; anything left over from
	// a redirect is picked up here. Reading the cookie either way clears it, so
	// a stale message cannot surface on the next page.
	if cookieFlash := takeFlash(w, r); data.Flash == nil {
		data.Flash = cookieFlash
	}
	if data.Data == nil {
		data.Data = map[string]any{}
	}
	if session, user, ok := sessionFrom(r); ok {
		data.User, data.LoggedIn = user, true
		if data.CSRF == "" {
			data.CSRF = session.CSRFToken
		}
	}
	if data.CSRF == "" {
		data.CSRF = s.issuePreSessionCSRF(w, r)
	}

	// Rendered into a buffer first: a template error half way through would
	// otherwise leave a broken page with a 200 already sent.
	var buffer bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buffer, "layout.html", data); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages show redacted connection strings, bucket names, admin emails and run
	// history. Without this they sit in the browser cache and come back on the
	// back button after a sign-out.
	w.Header().Set("Cache-Control", "no-store, private")
	// The security headers themselves are set by securityHeaders for every
	// response, not just this one — a 500, a 404 or a static asset needs them
	// just as much, and previously none of them had any.
	_, _ = buffer.WriteTo(w)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "Something went wrong. Check the feather log for details.", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Not found.", http.StatusNotFound)
}

// notFoundOrError picks the status an id lookup deserves.
//
// The POST action handlers all called serverError unconditionally, so a stale
// link to a config someone else deleted produced a 500 and a log line reading
// "request failed" — as if feather were broken rather than the row being gone.
// A 404 is both the truthful answer and the one that stops a monitored error
// rate rising every time a bookmark goes stale.
func (s *Server) notFoundOrError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	s.serverError(w, r, err)
}

// --- flash messages --------------------------------------------------------

const flashCookie = "feather_flash"

func (s *Server) setFlash(w http.ResponseWriter, kind, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    url.QueryEscape(kind + "|" + message),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	cookie, err := r.Cookie(flashCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})

	decoded, err := url.QueryUnescape(cookie.Value)
	if err != nil {
		return nil
	}
	kind, message, found := strings.Cut(decoded, "|")
	if !found {
		return nil
	}
	return &flash{Kind: kind, Message: message}
}

// --- template helpers ------------------------------------------------------

var templateFuncs = template.FuncMap{
	"bytes":       config.HumanBytes,
	"number":      humanNumber,
	"datetime":    formatDateTime,
	"date":        formatDate,
	"since":       humanSince,
	"duration":    humanDuration,
	"yesNo":       func(v bool) string { return map[bool]string{true: "yes", false: "no"}[v] },
	"statusTone":  statusTone,
	"auditLabel":  auditLabel,
	"prettyJSON":  prettyJSON,
	"compactJSON": compactJSON,
	"truncate":    truncateText,
	"keyTokens":   func() []config.KeyTemplateToken { return config.KeyTemplateTokens },
	"timezones":   commonTimezones,
	"readPreferences": func() []string {
		return []string{"primary", "primaryPreferred", "secondary", "secondaryPreferred", "nearest"}
	},
	"contains":    func(list []string, want string) bool { return sliceContains(list, want) },
	"queryString": queryString,
	"dict":        dict,
	"add":         func(a, b int) int { return a + b },
	"sub":         func(a, b int) int { return a - b },
	"percent":     percent,
}

func humanNumber(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	digits := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func formatDateTime(t any) string {
	switch value := t.(type) {
	case time.Time:
		if value.IsZero() {
			return "—"
		}
		return value.Local().Format("2 Jan 2006, 15:04")
	case *time.Time:
		if value == nil {
			return "—"
		}
		return formatDateTime(*value)
	default:
		return "—"
	}
}

func formatDate(t any) string {
	switch value := t.(type) {
	case time.Time:
		if value.IsZero() {
			return "—"
		}
		return value.Local().Format("2 Jan 2006")
	case *time.Time:
		if value == nil {
			return "—"
		}
		return formatDate(*value)
	default:
		return "—"
	}
}

func humanSince(t any) string {
	var at time.Time
	switch value := t.(type) {
	case time.Time:
		at = value
	case *time.Time:
		if value == nil {
			return "never"
		}
		at = *value
	default:
		return "never"
	}
	if at.IsZero() {
		return "never"
	}

	elapsed := time.Since(at)
	if elapsed < 0 {
		return "in " + humanDuration(-elapsed)
	}
	return humanDuration(elapsed) + " ago"
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "less than a second"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(math.Mod(d.Seconds(), 60)))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(math.Mod(d.Minutes(), 60)))
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())%24)
	}
}

// statusTone maps a status onto the colour vocabulary the stylesheet uses.
func statusTone(status any) string {
	switch fmt.Sprint(status) {
	case "success", "verified", "ok":
		return "good"
	case "running", "uploaded", "pending":
		return "busy"
	case "partial", "dry_run", "skipped":
		return "warn"
	case "failed":
		return "bad"
	default:
		return "muted"
	}
}

func prettyJSON(raw any) string {
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(fmt.Sprint(raw)), "", "  "); err != nil {
		return fmt.Sprint(raw)
	}
	return out.String()
}

func compactJSON(raw any) string {
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(fmt.Sprint(raw))); err != nil {
		return fmt.Sprint(raw)
	}
	return out.String()
}

func truncateText(length int, value any) string {
	text := fmt.Sprint(value)
	if len(text) <= length {
		return text
	}
	return text[:length] + "…"
}

func sliceContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func queryString(pairs ...any) template.URL {
	values := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		value := fmt.Sprint(pairs[i+1])
		if value == "" || value == "0" {
			continue
		}
		values.Set(fmt.Sprint(pairs[i]), value)
	}
	if len(values) == 0 {
		return ""
	}
	return template.URL("?" + values.Encode())
}

func dict(pairs ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[fmt.Sprint(pairs[i])] = pairs[i+1]
	}
	return out
}

func percent(part, whole int64) int {
	if whole == 0 {
		return 0
	}
	return int(float64(part) / float64(whole) * 100)
}

// commonTimezones is the short list offered in the schedule picker; any IANA
// name can still be typed.
func commonTimezones() []string {
	return []string{
		"UTC", "Asia/Kolkata", "Asia/Dubai", "Asia/Singapore", "Asia/Tokyo",
		"Europe/London", "Europe/Berlin", "America/New_York", "America/Chicago",
		"America/Los_Angeles", "Australia/Sydney",
	}
}
