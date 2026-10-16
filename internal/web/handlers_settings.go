package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

	selfBackup, err := s.app.SelfBackupConfig(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	selfBackups, err := app.ListSelfBackups(selfBackup.Dir)
	if err != nil {
		// Listing is informational; an unreadable directory is shown as an
		// error on the card rather than taking the whole page down.
		s.log.Warn("could not list database backups", "dir", selfBackup.Dir, "error", err)
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
			"WebhookURL":      webhookURL,
			"NotifyOnSuccess": notifyOnSuccess == 1,
			"MasterKeyPath":   s.app.Env.MasterKeyPath,
			"MasterKeyEnv":    s.app.Env.MasterKey != "",
			"Host":            s.app.Host,
			"SelfBackup":      selfBackup,
			"SelfBackups":     selfBackups,
			"SelfBackupError": errString(err),
			"SelfBackupMax":   app.MaxSelfBackupKeep,
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
	s.setFlash(w, "ok", "Settings saved. A change to concurrent jobs takes effect when Feather restarts.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleSelfBackupSave stores the automatic database backup settings and
// applies them at once: today's snapshot is taken if it is missing, and a
// lowered count prunes straight away rather than at the next hourly check.
func (s *Server) handleSelfBackupSave(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	keep := f.Int("selfBackupKeep", 0)
	if keep < 0 || keep > app.MaxSelfBackupKeep {
		s.setFlash(w, "error", "Backups to keep must be between 0 and %d.", app.MaxSelfBackupKeep)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	dir := strings.TrimSpace(f.Raw("selfBackupDir"))
	if dir != "" && !filepath.IsAbs(dir) {
		// Relative to what would depend on the working directory of whatever
		// started the process, which is not something to discover at restore time.
		s.setFlash(w, "error", "The backup directory must be an absolute path, such as /var/backups/feather.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	if err := s.app.Store.SetMeta(r.Context(), app.MetaSelfBackupKeep, strconv.Itoa(keep)); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.app.Store.SetMeta(r.Context(), app.MetaSelfBackupDir, dir); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.audit(r, store.AuditSettingsSaved, "instance", "database backups", "")

	if keep == 0 {
		s.setFlash(w, "ok", "Automatic database backups are off. Existing backups were left in place.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if _, err := s.app.RunSelfBackup(r.Context()); err != nil {
		s.log.Error("database backup failed", "error", err)
		s.setFlash(w, "error", "Settings saved, but the backup could not be written: %s", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	s.setFlash(w, "ok", "Saved. Feather keeps one database backup a day, the newest %d.", keep)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleSelfBackupRun takes today's snapshot now, if it has not been taken.
func (s *Server) handleSelfBackupRun(w http.ResponseWriter, r *http.Request) {
	settings, err := s.app.SelfBackupConfig(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !settings.Enabled() {
		s.setFlash(w, "error", "Automatic database backups are off. Set how many to keep first.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	written, err := s.app.RunSelfBackup(r.Context())
	if err != nil {
		s.log.Error("database backup failed", "error", err)
		s.setFlash(w, "error", "The backup could not be written: %s", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if written == nil {
		s.setFlash(w, "ok", "Today's backup already exists. Feather keeps one per day.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	s.audit(r, store.AuditDatabaseBackedUp, "instance", written.Path, "")
	s.setFlash(w, "ok", "Wrote %s (%s).", written.Name, humanBytes(written.Size))
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

// handleMasterKeyDownload hands the master key over as a file, so it can be
// kept with the backups that cannot be read without it.
//
// The current password is asked for again. A session left open on someone's
// laptop can already change retention rules, but it should not be able to walk
// away with the one thing that turns a stolen database file into every
// connection string and cloud secret in it.
func (s *Server) handleMasterKeyDownload(w http.ResponseWriter, r *http.Request) {
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
	if err := s.app.Store.CheckPassword(r.Context(), user.ID, f.Raw("password")); err != nil {
		s.log.Warn("master key download refused", "email", user.Email, "ip", s.clientIP(r))
		s.setFlash(w, "error", "That password is not correct. The master key was not downloaded.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	// Served from wherever the running process got it, so what is downloaded
	// is exactly the key that opens this database.
	key, source := strings.TrimSpace(s.app.Env.MasterKey), "FEATHER_MASTER_KEY"
	if key == "" {
		data, err := os.ReadFile(s.app.Env.MasterKeyPath)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		key, source = strings.TrimSpace(string(data)), s.app.Env.MasterKeyPath
	}

	s.audit(r, store.AuditMasterKeyDownloaded, "instance", source, "")
	s.log.Warn("master key downloaded", "by", user.Email, "ip", s.clientIP(r), "source", source)

	body := key + "\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="feather-master.key"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write([]byte(body))
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// boolMeta stores a checkbox as the "1"/"0" the meta table uses.
func boolMeta(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
