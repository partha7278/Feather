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
	"log/slog"
	"net"
	"net/http"
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
}

// New builds the HTTP server. secureCookies marks session cookies Secure,
// which is right whenever feather is reached over HTTPS — directly or through
// a reverse proxy.
func New(application *app.App, log *slog.Logger, secureCookies bool) (*Server, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{app: application, log: log, templates: templates, secure: secureCookies}, nil
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
	mux.HandleFunc("GET /runs", s.guard(s.handleRuns))
	mux.HandleFunc("GET /runs/{id}", s.guard(s.handleRunDetail))

	mux.HandleFunc("GET /settings", s.guard(s.handleSettings))
	mux.HandleFunc("POST /settings", s.guard(s.handleSettingsSave))
	mux.HandleFunc("POST /settings/password", s.guard(s.handlePasswordChange))
	mux.HandleFunc("POST /settings/admins", s.guard(s.handleAdminCreate))
	mux.HandleFunc("POST /settings/admins/{id}/delete", s.guard(s.handleAdminDelete))
	mux.HandleFunc("POST /settings/export", s.guard(s.handleSettingsExport))
	mux.HandleFunc("POST /settings/import", limitBody(maxBundleBytes, s.guard(s.handleSettingsImport)))

	mux.HandleFunc("GET /api/collections", s.guard(s.handleAPICollections))

	return s.recoverPanics(s.logRequests(s.setupGate(mux)))
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
			"duration", time.Since(started).Round(time.Millisecond), "ip", clientIP(r))
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

func clientIP(r *http.Request) string {
	// A reverse proxy is the normal deployment, so trust its header when there
	// is one; feather binds to localhost by default.
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// reload pushes freshly saved settings into the running scheduler and clients.
// A failure here is worth surfacing: the change is stored but not yet live.
func (s *Server) reload(r *http.Request) error {
	return s.app.Reload(r.Context())
}

// triggerForUser records that a run was started by a person in the UI rather
// than by the scheduler.
const uiTrigger = store.TriggerUI
