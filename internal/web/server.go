// Package web is feather's control panel: the first-run setup, the MongoDB and
// backup accounts, the retention configs, and the catalogue of every archive
// written so far.
//
// It is server-rendered HTML with ordinary form posts. Every mutation works
// without JavaScript; the small script that ships with it only smooths edges
// (live file name previews, confirmation prompts, in-place connection tests).
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/store"
)

//go:embed static
var staticFS embed.FS

// Server serves the control panel.
type Server struct {
	app       *app.App
	log       *slog.Logger
	templates *templateSet
	secure    bool

	// trustedProxies are the networks whose forwarded-for headers are believed.
	// Empty means none, and then only the socket address is ever used.
	trustedProxies []netip.Prefix
}

// Options configures the server.
type Options struct {
	// SecureCookies marks session cookies Secure. Leave it nil to derive the
	// answer from BaseURL, which is what an operator expects: a panel published
	// over HTTPS should not need a second flag saying so.
	SecureCookies *bool
	// BaseURL is the externally visible URL, when feather is behind a proxy.
	BaseURL string
	// TrustedProxies is a comma-separated list of CIDRs or addresses whose
	// X-Forwarded-For, Forwarded and X-Real-IP headers are believed.
	TrustedProxies string
}

// New builds the HTTP server.
func New(application *app.App, log *slog.Logger, opts Options) (*Server, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}

	// Secure cookies default to on whenever the panel is published over HTTPS.
	// They used to default to off and need --secure-cookies, and the shipped
	// systemd unit and Dockerfile both omitted it — so the documented
	// deployment served session cookies without the Secure attribute over a
	// connection that was, in fact, TLS.
	secure := strings.HasPrefix(strings.ToLower(opts.BaseURL), "https://")
	if opts.SecureCookies != nil {
		secure = *opts.SecureCookies
	}

	proxies, err := parseTrustedProxies(opts.TrustedProxies)
	if err != nil {
		return nil, err
	}
	if len(proxies) > 0 {
		log.Info("trusting forwarded-for headers from", "networks", opts.TrustedProxies)
	}

	return &Server{
		app: application, log: log, templates: templates,
		secure: secure, trustedProxies: proxies,
	}, nil
}

// parseTrustedProxies accepts CIDRs and bare addresses, plus the shorthands
// "local" (loopback, where a proxy on the same host connects from) and
// "private" (the RFC 1918 and unique-local ranges, for a proxy in the same
// Docker network or VPC).
func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		switch strings.ToLower(entry) {
		case "local", "loopback":
			out = append(out, netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"))
			continue
		case "private":
			for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"} {
				out = append(out, netip.MustParsePrefix(cidr))
			}
			continue
		}

		if prefix, err := netip.ParsePrefix(entry); err == nil {
			out = append(out, prefix)
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q is not an IP address or CIDR block", entry)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// Handler builds the routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Everything below requires a session.
	mux.HandleFunc("GET /{$}", s.guard(s.handleDashboard))

	mux.HandleFunc("GET /mongo", s.guard(s.handleMongoList))
	mux.HandleFunc("GET /mongo/new", s.guard(s.handleMongoForm))
	mux.HandleFunc("GET /mongo/{id}", s.guard(s.handleMongoForm))
	mux.HandleFunc("POST /mongo", s.guard(s.handleMongoSave))
	mux.HandleFunc("POST /mongo/{id}", s.guard(s.handleMongoSave))
	mux.HandleFunc("POST /mongo/{id}/test", s.guard(s.handleMongoTest))
	mux.HandleFunc("POST /mongo/{id}/delete", s.guard(s.handleMongoDelete))

	mux.HandleFunc("GET /destinations", s.guard(s.handleDestinationList))
	mux.HandleFunc("GET /destinations/new", s.guard(s.handleDestinationForm))
	mux.HandleFunc("GET /destinations/{id}", s.guard(s.handleDestinationForm))
	mux.HandleFunc("POST /destinations", s.guard(s.handleDestinationSave))
	mux.HandleFunc("POST /destinations/{id}", s.guard(s.handleDestinationSave))
	mux.HandleFunc("POST /destinations/{id}/test", s.guard(s.handleDestinationTest))
	mux.HandleFunc("POST /destinations/{id}/delete", s.guard(s.handleDestinationDelete))
	mux.HandleFunc("GET "+gdriveCallbackPath, s.guard(s.handleGDriveCallback))
	mux.HandleFunc("POST /destinations/{id}/gdrive/connect", s.guard(s.handleGDriveConnect))
	mux.HandleFunc("POST /destinations/{id}/gdrive/disconnect", s.guard(s.handleGDriveDisconnect))

	mux.HandleFunc("GET /configs", s.guard(s.handleConfigList))
	mux.HandleFunc("GET /configs/new", s.guard(s.handleConfigForm))
	mux.HandleFunc("GET /configs/{id}", s.guard(s.handleConfigForm))
	mux.HandleFunc("POST /configs", s.guard(s.handleConfigSave))
	mux.HandleFunc("POST /configs/{id}", s.guard(s.handleConfigSave))
	mux.HandleFunc("POST /configs/{id}/enabled", s.guard(s.handleConfigEnabled))
	mux.HandleFunc("POST /configs/{id}/deletion", s.guard(s.handleConfigDeletion))
	mux.HandleFunc("POST /configs/{id}/run", s.guard(s.handleConfigRun))
	mux.HandleFunc("POST /configs/{id}/preview", s.guard(s.handleConfigPreview))
	mux.HandleFunc("POST /configs/{id}/delete", s.guard(s.handleConfigDelete))

	mux.HandleFunc("GET /backups", s.guard(s.handleBackups))
	mux.HandleFunc("GET /backups/{id}/download", s.guard(s.handleArtifactDownload))
	mux.HandleFunc("GET /runs", s.guard(s.handleRuns))
	mux.HandleFunc("GET /audit", s.guard(s.handleAudit))
	mux.HandleFunc("GET /runs/{id}", s.guard(s.handleRunDetail))

	mux.HandleFunc("GET /settings", s.guard(s.handleSettings))
	mux.HandleFunc("POST /settings", s.guard(s.handleSettingsSave))
	mux.HandleFunc("POST /settings/password", s.guard(s.handlePasswordChange))
	mux.HandleFunc("POST /settings/admins", s.guard(s.handleAdminCreate))
	mux.HandleFunc("POST /settings/admins/{id}/delete", s.guard(s.handleAdminDelete))
	mux.HandleFunc("POST /settings/backup", s.guard(s.handleSettingsBackup))
	mux.HandleFunc("POST /settings/export", s.guard(s.handleSettingsExport))
	mux.HandleFunc("POST /settings/import", limitBody(maxBundleBytes, s.guard(s.handleSettingsImport)))

	mux.HandleFunc("GET /api/collections", s.guard(s.handleAPICollections))

	return s.recoverPanics(s.securityHeaders(s.limitRequestBody(s.logRequests(s.setupGate(mux)))))
}

// maxFormBytes bounds an ordinary form post. The largest legitimate one is a
// retention config carrying a filter and a projection, which is kilobytes; a
// megabyte is generous. The bundle import has its own, larger limit applied at
// its route, and is the only thing that needs one.
const maxFormBytes = 1 << 20

// limitRequestBody caps how much feather will read from a request.
//
// Only /settings/import was capped before, so every other POST could stream as
// much as it liked for the full two-minute read timeout — into a process that
// is also running the archives. ParseForm buffers what it reads.
func (s *Server) limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
			next.ServeHTTP(w, r)
			return
		}
		// The bundle route sets its own, larger limit; applying this one first
		// would override it, since MaxBytesReader does not nest.
		if r.URL.Path == "/settings/import" {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		next.ServeHTTP(w, r)
	})
}

// securityHeaders applies the response headers to everything.
//
// They used to be set inside render(), which meant they were on rendered pages
// and nowhere else: not on http.Error responses, not on the panic handler, not
// on the 405s the mux produces for a POST-only route, not on /healthz, and not
// on /static. A CSP that covers the pages but not the error pages is a CSP with
// a hole in it, and the hole is exactly where injected content would land.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		// frame-ancestors is the modern control; X-Frame-Options is kept for
		// the scanners and proxies that only know the old one.
		header.Set("X-Frame-Options", "DENY")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=()")
		// The panel serves only its own embedded assets.
		header.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; "+
				"object-src 'none'")

		// Only over HTTPS, and only when feather knows it is: promising HSTS on
		// a plain-HTTP deployment locks a browser out of a panel that has no
		// TLS to fall back on.
		if s.secure {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		// Embedded assets are content-addressed by release, not by URL, so a
		// long cache would serve last version's CSS after an upgrade. An hour
		// is enough to stop the refetch-per-page-load that no headers at all
		// produced, and short enough that an upgrade settles quickly.
		if strings.HasPrefix(r.URL.Path, "/static/") {
			header.Set("Cache-Control", "public, max-age=3600")
		}

		next.ServeHTTP(w, r)
	})
}

// Serve runs the HTTP server until the context is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Uploads here are tiny forms, but a retention config's filter can be
		// long, and an operator on a slow link should not be cut off.
		ReadTimeout:  2 * time.Minute,
		WriteTimeout: 2 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		s.log.Info("control panel listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// setupGate forces a fresh installation through the first-run journey. Until an
// administrator exists there is nothing to protect and nothing to show, so
// every path leads to /setup; afterwards, /setup itself stops existing.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/healthz" || strings.HasPrefix(path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		needsSetup, err := s.app.Store.NeedsSetup(r.Context())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		switch {
		case needsSetup && path != "/setup":
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
		case !needsSetup && path == "/setup":
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		s.log.Debug("http",
			"method", r.Method, "path", r.URL.Path, "status", recorder.status,
			"duration", time.Since(started).Round(time.Millisecond), "ip", s.clientIP(r))
	})
}

// recoverPanics keeps one bad page from taking the daemon down with it — the
// same process is running the archives.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", recovered)
				http.Error(w, "Something went wrong.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// clientIP is the address of whoever made this request, as far as it can be
// established.
//
// The forwarded headers are only read when the immediate peer is a trusted
// proxy. They used to be read unconditionally, which meant anything downstream
// could claim any address: the login throttle keyed on it, so a random value
// per request bypassed it entirely, and the addresses in the log were whatever
// the client felt like writing.
//
// With no trusted proxies configured, only the socket address is used. That is
// the correct default: feather binds to localhost, and an operator putting it
// behind a proxy is the one who knows the proxy's address.
func (s *Server) clientIP(r *http.Request) string {
	peer := peerIP(r)
	if !s.trustsPeer(peer) {
		return peer
	}

	// Forwarded (RFC 7239) first, then the de-facto X-Forwarded-For. The
	// left-most entry is the original client; entries to its right were added
	// by successive proxies.
	if forwarded := r.Header.Get("Forwarded"); forwarded != "" {
		if first, _, _ := strings.Cut(forwarded, ","); first != "" {
			for _, part := range strings.Split(first, ";") {
				key, value, found := strings.Cut(strings.TrimSpace(part), "=")
				if found && strings.EqualFold(key, "for") {
					if addr := normaliseForwardedFor(value); addr != "" {
						return addr
					}
				}
			}
		}
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if addr := strings.TrimSpace(first); addr != "" {
			return addr
		}
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return peer
}

// trustsProxy reports whether this request came through a proxy feather has
// been told to believe. Used for logging, so an operator can tell whether the
// address in a line is authoritative.
func (s *Server) trustsProxy(r *http.Request) bool { return s.trustsPeer(peerIP(r)) }

func (s *Server) trustsPeer(peer string) bool {
	if len(s.trustedProxies) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	for _, network := range s.trustedProxies {
		if network.Contains(addr) {
			return true
		}
	}
	return false
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// normaliseForwardedFor unwraps the quoting and bracketing RFC 7239 allows:
// for="[2001:db8::1]:443", for=192.0.2.60:1234, for="_hidden".
func normaliseForwardedFor(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" || strings.HasPrefix(value, "_") || value == "unknown" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.Trim(value, "[]")
}

// reload pushes freshly saved settings into the running scheduler and clients.
// A failure here is worth surfacing: the change is stored but not yet live.
func (s *Server) reload(r *http.Request) error {
	return s.app.Reload(r.Context())
}

// triggerForUser records that a run was started by a person in the UI rather
// than by the scheduler.
const uiTrigger = store.TriggerUI
