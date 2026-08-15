package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

// Connecting a Google Drive destination is the one place feather sends an
// operator off-site and back. Drive will not hand out a refresh token to a
// pasted password, so the account is saved first with its OAuth client, then
// authorised here: feather redirects to Google, Google redirects back to
// gdriveCallbackPath with a code, and the refresh token that code buys is
// sealed into the destination.
const (
	gdriveCallbackPath = "/destinations/gdrive/callback"
	gdriveStateCookie  = "feather_gdrive_state"
)

// handleGDriveConnect starts the authorisation round trip for one destination.
func (s *Server) handleGDriveConnect(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Store.DestinationByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}

	if record.Provider != config.ProviderGDrive || record.OAuthClientID == "" || record.OAuthClientSecret == "" {
		s.setFlash(w, "error", "%s has no Google OAuth client to connect. Add a client ID and secret first.", record.Alias)
		http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
		return
	}

	nonce, err := randomToken()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The state carries which destination is being connected and a nonce that
	// is also set as a cookie, so the callback can prove the round trip started
	// here rather than on some other page.
	state := record.ID + ":" + nonce
	http.SetCookie(w, &http.Cookie{
		Name:     gdriveStateCookie,
		Value:    state,
		Path:     gdriveCallbackPath,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	auth := s.gdriveOAuthConfig(r, record).AuthCodeURL(state,
		// Offline access is what produces a refresh token at all, and asking
		// for consent every time is what makes Google send a new one even when
		// this account has been authorised before.
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	)
	http.Redirect(w, r, auth, http.StatusSeeOther)
}

// handleGDriveCallback finishes the round trip: it exchanges the code for a
// refresh token and stores it against the destination.
func (s *Server) handleGDriveCallback(w http.ResponseWriter, r *http.Request) {
	s.clearGDriveState(w)

	query := r.URL.Query()
	state := query.Get("state")
	cookie, err := r.Cookie(gdriveStateCookie)
	if err != nil || cookie.Value == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		s.setFlash(w, "error", "That Google authorisation did not come back to the page it started from. Try connecting again.")
		http.Redirect(w, r, "/destinations", http.StatusSeeOther)
		return
	}

	id, _, _ := strings.Cut(state, ":")
	record, err := s.app.Store.DestinationByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}

	// Google reports a refusal in the URL rather than as an HTTP error.
	if failure := query.Get("error"); failure != "" {
		s.setFlash(w, "error", "Google did not authorise %s: %s", record.Alias, failure)
		http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
		return
	}

	token, err := s.gdriveOAuthConfig(r, record).Exchange(r.Context(), query.Get("code"))
	if err != nil {
		s.setFlash(w, "error", "Could not complete the Google sign-in: %s", err)
		http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
		return
	}
	if token.RefreshToken == "" {
		// Without a refresh token feather could upload today and fail in an
		// hour, which is worse than failing now.
		s.setFlash(w, "error", "Google returned an access token but no refresh token, so %s could not be connected. Remove feather from the account's third-party access and try again.", record.Alias)
		http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
		return
	}

	record.OAuthRefreshToken = token.RefreshToken
	// Saving through the store re-seals the token and re-validates the account.
	if err := s.app.Store.SaveDestination(r.Context(), &record); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("connected, but could not reload configuration", "error", err)
	}
	s.log.Info("google drive destination connected", "destination", record.Alias)

	s.setFlash(w, "ok", "Connected %s to Google Drive. Test it to confirm the folder is writable.", record.Alias)
	http.Redirect(w, r, destinationFormPath(record.ID), http.StatusSeeOther)
}

// handleGDriveDisconnect forgets the stored refresh token, which parks the
// destination until it is connected again.
func (s *Server) handleGDriveDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.ClearDriveAuthorisation(r.Context(), r.PathValue("id")); err != nil {
		s.setFlash(w, "error", "%s", err.Error())
		http.Redirect(w, r, "/destinations", http.StatusSeeOther)
		return
	}
	if err := s.reload(r); err != nil {
		s.log.Warn("disconnected, but could not reload configuration", "error", err)
	}

	s.setFlash(w, "ok", "Disconnected from Google Drive. Archives already written stay where they are.")
	s.redirectBack(w, r, "/destinations")
}

func (s *Server) gdriveOAuthConfig(r *http.Request, record store.DestinationRecord) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     record.OAuthClientID,
		ClientSecret: record.OAuthClientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  s.gdriveRedirectURL(r),
		// Full Drive access: the folder an operator names already exists and was
		// not created by feather, and the narrower drive.file scope cannot see
		// anything this application did not create itself.
		Scopes: []string{drive.DriveScope},
	}
}

// gdriveRedirectURL is the address Google sends the browser back to, and the
// one that has to be registered on the OAuth client. It follows the configured
// base URL when there is one, since behind a reverse proxy the request's own
// host is the only other thing to go on.
func (s *Server) gdriveRedirectURL(r *http.Request) string {
	if base := s.app.Env.BaseURL; base != "" {
		return strings.TrimSuffix(base, "/") + gdriveCallbackPath
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: gdriveCallbackPath}).String()
}

func (s *Server) clearGDriveState(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     gdriveStateCookie,
		Value:    "",
		Path:     gdriveCallbackPath,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// driveFolderID accepts what an operator actually has to hand: a folder ID, or
// the browser URL of the folder they are looking at.
func driveFolderID(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "/") {
		return raw
	}
	if parsed, err := url.Parse(raw); err == nil {
		if id := parsed.Query().Get("id"); id != "" {
			return id
		}
		segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		return segments[len(segments)-1]
	}
	return raw
}
