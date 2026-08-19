package web

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/bundle"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	admins, err := s.app.Store.ListAdmins(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	webhookURL, err := s.app.Store.Meta(r.Context(), app.MetaWebhookURL, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	notifyOnSuccess, err := s.app.Store.MetaInt(r.Context(), app.MetaNotifyOnSuccess, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	instanceName, err := s.app.Store.Meta(r.Context(), app.MetaInstanceName, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.render(w, r, "settings", pageData{
		Title: "Settings",
		Nav:   "settings",
		Data: map[string]any{
			"Admins":          admins,
			"InstanceName":    instanceName,
			"MaxConcurrent":   s.app.MaxConcurrentJobs(r.Context()),
			"MinPassword":     store.MinPasswordLength,
			"MinPassphrase":   bundle.MinPassphraseLength,
			"DatabasePath":    s.app.Store.Path(),
			"SpoolDir":        s.app.Env.SpoolDir,
			"DataDir":         s.app.Env.DataDir,
			"WebhookURL":      webhookURL,
			"NotifyOnSuccess": notifyOnSuccess == 1,
			"MasterKeyPath":   s.app.Env.MasterKeyPath,
			"MasterKeyEnv":    s.app.Env.MasterKey != "",
			"Host":            s.app.Host,
		},
	})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	maxConcurrent := f.Int("maxConcurrent", s.app.Env.MaxConcurrentJobs)
	if maxConcurrent < 1 || maxConcurrent > 16 {
		s.setFlash(w, "error", "Concurrent jobs must be between 1 and 16.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	if err := s.app.Store.SetMeta(r.Context(), app.MetaInstanceName, f.String("instanceName")); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.app.Store.SetMeta(r.Context(), app.MetaMaxConcurrentJobs, strconv.Itoa(maxConcurrent)); err != nil {
		s.serverError(w, r, err)
		return
	}

	webhook := strings.TrimSpace(f.Raw("webhookURL"))
	if webhook != "" {
		// Checked here rather than at delivery time, where the only symptom
		// would be a warning in a log nobody is reading — which is the exact
		// failure notifications exist to prevent.
		parsed, err := url.Parse(webhook)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			s.setFlash(w, "error", "The notification webhook must be a full http:// or https:// URL.")
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
	}
	if err := s.app.Store.SetMeta(r.Context(), app.MetaWebhookURL, webhook); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.app.Store.SetMeta(r.Context(), app.MetaNotifyOnSuccess, boolMeta(f.Bool("notifyOnSuccess"))); err != nil {
		s.serverError(w, r, err)
		return
	}

	if err := s.app.Reload(r.Context()); err != nil {
		s.log.Error("settings saved but could not be applied", "error", err)
		s.setFlash(w, "error", "Settings were saved but could not be applied to the running process: %s",
			config.RedactSecrets(err.Error()))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	// The concurrency cap is fixed when the scheduler is built, so it is the one
	// setting here that genuinely needs a restart.
	s.audit(r, store.AuditSettingsSaved, "instance", "", "")
	s.setFlash(w, "ok", "Settings saved. A change to concurrent jobs takes effect when feather restarts.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	_, user, ok := sessionFrom(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	current := f.Raw("currentPassword")
	next := f.Raw("newPassword")
	if next != f.Raw("confirmPassword") {
		s.setFlash(w, "error", "The two new passwords do not match.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	if err := s.app.Store.ChangePassword(r.Context(), user.ID, current, next); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.log.Info("password changed", "email", user.Email, "ip", s.clientIP(r))
	s.audit(r, store.AuditPasswordChanged, "admin", user.Email, "")

	// Changing a password invalidates every session that account has, this one
	// included — the point of doing it is usually that one of them might not be
	// yours. Clear the cookie so the next request is a clean login rather than
	// a bounce off the guard.
	s.clearSessionCookie(w)
	s.setFlash(w, "ok", "Password changed. Every session for this account was signed out, including this one — please sign in again.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	email := f.Require("email")
	password := f.Raw("password")
	if !f.OK() {
		s.setFlash(w, "error", "%s", f.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	user, err := s.app.Store.CreateAdmin(r.Context(), email, password)
	if err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.log.Info("administrator added", "email", user.Email, "by", currentUserEmail(r))
	s.audit(r, store.AuditAdminCreated, "admin", user.Email, "")
	s.setFlash(w, "ok", "Added %s as an administrator.", user.Email)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	_, user, _ := sessionFrom(r)
	id := r.PathValue("id")

	if id == user.ID {
		s.setFlash(w, "error", "You cannot remove the account you are signed in with.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if err := s.app.Store.DeleteAdmin(r.Context(), id); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.audit(r, store.AuditAdminDeleted, "admin", id, "")
	s.log.Info("administrator removed", "id", id, "by", user.Email)
	s.setFlash(w, "ok", "Administrator removed.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleSettingsBackup writes a consistent snapshot of feather's own database.
//
// This is deliberately separate from Export. An export bundle is a portable
// copy of the configuration — accounts, destinations, retention rules — and is
// what you carry to another machine. A backup is the whole state, catalogue and
// administrator logins included, and is what you restore from after losing the
// server. The README used to describe copying the file, which is wrong while
// feather is running: this takes the snapshot properly.
func (s *Server) handleSettingsBackup(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	path := strings.TrimSpace(f.Raw("path"))
	if path == "" {
		path = filepath.Join(filepath.Dir(s.app.Store.Path()),
			"feather-backup-"+time.Now().UTC().Format("20060102T150405Z")+".db")
	}
	if !filepath.IsAbs(path) {
		s.setFlash(w, "error", "Give the full path to write the backup to.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	size, err := s.app.Store.Backup(r.Context(), path)
	if err != nil {
		s.setFlash(w, "error", "%s", config.RedactSecrets(err.Error()))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.audit(r, store.AuditDatabaseBackedUp, "instance", path, humanBytes(size))
	s.log.Info("database backup written", "by", currentUserEmail(r), "path", path, "bytes", size)

	// Said every time. A backup without the key cannot be opened, and that is
	// the failure people discover at the worst possible moment.
	s.setFlash(w, "success", "Wrote %s (%s). It needs the master key at %s to be read — keep a copy of that key with it.",
		path, humanBytes(size), s.app.Env.MasterKeyPath)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// humanBytes formats a size the way an operator reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// boolMeta stores a checkbox as the "1"/"0" the meta table uses.
func boolMeta(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
