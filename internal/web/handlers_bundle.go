package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/unifers/feather/internal/bundle"
)

// maxBundleBytes bounds an uploaded bundle. A large installation is a few
// hundred kilobytes of JSON; this leaves room to spare and still refuses to
// read an arbitrary file into memory.
const maxBundleBytes = 8 << 20

// handleSettingsExport writes the whole installation out as one file. Unless
// the operator asks for a bundle without credentials, it is sealed under a
// passphrase they type here — the master key does not travel with it.
func (s *Server) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	withoutSecrets := f.Bool("withoutSecrets")
	passphrase := f.Raw("passphrase")

	if !withoutSecrets {
		switch {
		case passphrase == "":
			s.setFlash(w, "error", "A passphrase is required to export credentials. Tick “without credentials” to export without them.")
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		case passphrase != f.Raw("passphraseConfirm"):
			s.setFlash(w, "error", "The two passphrases do not match.")
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
	}

	exported, err := bundle.Export(r.Context(), s.app, bundle.ExportOptions{
		Passphrase:     passphrase,
		WithoutSecrets: withoutSecrets,
	})
	if err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	body, err := exported.Encode()
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.log.Info("configuration exported",
		"by", currentUserEmail(r),
		"with_credentials", exported.HasSecrets(),
		"mongo_accounts", len(exported.MongoSources),
		"destinations", len(exported.Destinations),
		"configs", len(exported.Configs))

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exported.FileName()+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A bundle is the most sensitive thing feather will ever serve.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write(body)
}

// handleSettingsImport reads a bundle back in. By default it only adds what is
// missing, so importing the same file twice changes nothing the second time.
func (s *Server) handleSettingsImport(w http.ResponseWriter, r *http.Request) {
	file, header, err := r.FormFile("bundle")
	if err != nil {
		s.setFlash(w, "error", "Choose a bundle file to import.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBundleBytes))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	f, err := newForm(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	incoming, err := bundle.Decode(data)
	if err != nil {
		s.setFlash(w, "error", "%s could not be read: %s", header.Filename, err)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	result, err := incoming.Apply(r.Context(), s.app, bundle.ImportOptions{
		Passphrase: f.Raw("passphrase"),
		Overwrite:  f.Bool("overwrite"),
		Settings:   f.Bool("importSettings"),
	})
	if err != nil {
		switch {
		case errors.Is(err, bundle.ErrPassphraseRequired), errors.Is(err, bundle.ErrWrongPassphrase):
			s.setFlash(w, "error", "%s", err.Error())
		default:
			s.log.Error("import failed", "file", header.Filename, "error", err)
			s.setFlash(w, "error", "The import failed: %s", err.Error())
		}
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	if err := s.reload(r); err != nil {
		s.log.Warn("imported, but could not reload configuration", "error", err)
	}
	s.log.Info("configuration imported",
		"by", currentUserEmail(r), "file", header.Filename,
		"overwrite", f.Bool("overwrite"), "summary", result.Summary(), "notes", result.Notes)

	// The whole report can be long; the flash carries the counts and the first
	// few leftovers, and the log has all of them.
	message := result.Summary()
	if len(result.Notes) > 0 {
		message += " Not applied: " + strings.Join(firstFew(result.Notes, 3), "; ") + "."
	}
	if result.Applied() == 0 {
		s.setFlash(w, "info", "Nothing was imported. %s", message)
	} else {
		s.setFlash(w, "ok", "%s", message)
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func firstFew(notes []string, limit int) []string {
	if len(notes) <= limit {
		return notes
	}
	return append(notes[:limit:limit], fmt.Sprintf("and %d more (see the feather log)", len(notes)-limit))
}

// limitBody caps a request before anything downstream reads it, including the
// CSRF check — which parses the upload to find its token.
func limitBody(limit int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next(w, r)
	}
}
