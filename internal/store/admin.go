package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// SessionTTL is how long a browser login lasts before it must be repeated.
const SessionTTL = 12 * time.Hour

// MinPasswordLength is the floor for an admin password. This account can
// delete production data on a schedule, so the bar is a passphrase rather than
// a word.
const MinPasswordLength = 12

// bcryptCost is deliberately above the library default: logins are rare and a
// stolen database should be expensive to attack.
const bcryptCost = 12

// ErrBadCredentials covers both an unknown email and a wrong password, so a
// login form cannot be used to enumerate accounts.
var ErrBadCredentials = errors.New("that email and password combination is not recognised")

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)

// NeedsSetup reports whether the first-run journey still has to happen. Until
// an administrator exists, the UI shows nothing but the setup form.
func (s *Store) NeedsSetup(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

// CreateAdmin adds an administrator. The first one is created by the setup
// journey; later ones from the settings page.
func (s *Store) CreateAdmin(ctx context.Context, email, password string) (AdminUser, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if !emailRe.MatchString(email) {
		return AdminUser{}, errors.New("that does not look like an email address")
	}
	if err := CheckPasswordStrength(password); err != nil {
		return AdminUser{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return AdminUser{}, err
	}

	now := time.Now().UTC()
	user := AdminUser{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: string(hash),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO admin_users (id, email, password_hash, created_at, updated_at)
		VALUES (?,?,?,?,?)`,
		user.ID, user.Email, user.PasswordHash, timeText(now), timeText(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return AdminUser{}, fmt.Errorf("an administrator with the email %s already exists", email)
		}
		return AdminUser{}, err
	}
	return user, nil
}

// CheckPasswordStrength enforces the minimum an admin password must clear.
func CheckPasswordStrength(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", MinPasswordLength)
	}
	// bcrypt silently ignores anything past 72 bytes, which would make a long
	// passphrase weaker than it looks.
	if len(password) > 72 {
		return errors.New("the password must be at most 72 bytes")
	}
	return nil
}

// Authenticate checks an email and password pair.
func (s *Store) Authenticate(ctx context.Context, email, password string) (AdminUser, error) {
	user, err := s.AdminByEmail(ctx, strings.TrimSpace(strings.ToLower(email)))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Spend the time anyway; a fast rejection tells an attacker the email
			// is unknown.
			_ = bcrypt.CompareHashAndPassword(
				[]byte("$2a$12$oGCZQPPuY6cVMWk0lyKUJeaGvVeqGmZ.WnGvNPS7lIT/JqZQTgxOe"), []byte(password))
			return AdminUser{}, ErrBadCredentials
		}
		return AdminUser{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return AdminUser{}, ErrBadCredentials
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = ? WHERE id = ?`, nowText(), user.ID); err != nil {
		return user, err
	}
	return user, nil
}

// ChangePassword re-hashes an administrator's password after checking the
// current one, and drops every other session that account has open.
func (s *Store) ChangePassword(ctx context.Context, userID, current, next string) error {
	user, err := s.AdminByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(current)); err != nil {
		return errors.New("the current password is not correct")
	}
	if err := CheckPasswordStrength(next); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcryptCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		string(hash), nowText(), userID)
	return err
}

// ListAdmins returns every administrator, by email.
func (s *Store) ListAdmins(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, email, password_hash, created_at, updated_at, last_login_at
		FROM admin_users ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AdminUser
	for rows.Next() {
		user, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
	}
	return out, rows.Err()
}

func (s *Store) AdminByID(ctx context.Context, id string) (AdminUser, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, created_at, updated_at, last_login_at
		FROM admin_users WHERE id = ?`, id)
	user, err := scanAdmin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return user, ErrNotFound
	}
	return user, err
}

func (s *Store) AdminByEmail(ctx context.Context, email string) (AdminUser, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, created_at, updated_at, last_login_at
		FROM admin_users WHERE email = ?`, email)
	user, err := scanAdmin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return user, ErrNotFound
	}
	return user, err
}

// DeleteAdmin removes an administrator, refusing to remove the last one — an
// installation with no way in is not recoverable from the UI.
func (s *Store) DeleteAdmin(ctx context.Context, id string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return errors.New("this is the only administrator — create another one before removing it")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_users WHERE id = ?`, id)
	return err
}

// ResetPassword sets a password without knowing the old one. Reachable only
// from the command line, for the operator who is locked out.
func (s *Store) ResetPassword(ctx context.Context, email, password string) error {
	if err := CheckPasswordStrength(password); err != nil {
		return err
	}
	user, err := s.AdminByEmail(ctx, strings.TrimSpace(strings.ToLower(email)))
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		string(hash), nowText(), user.ID); err != nil {
		return err
	}
	// Any session opened with the old password is no longer trustworthy.
	_, err = s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE user_id = ?`, user.ID)
	return err
}

func scanAdmin(row interface{ Scan(...any) error }) (AdminUser, error) {
	var (
		user                 AdminUser
		createdAt, updatedAt string
		lastLogin            sql.NullString
	)
	if err := row.Scan(&user.ID, &user.Email, &user.PasswordHash, &createdAt, &updatedAt, &lastLogin); err != nil {
		return user, err
	}
	user.CreatedAt, _ = parseTime(createdAt)
	user.UpdatedAt, _ = parseTime(updatedAt)
	user.LastLoginAt = scanTime(lastLogin)
	return user, nil
}

// --- sessions --------------------------------------------------------------

// CreateSession opens a browser session and returns the cookie value. Only the
// hash is stored, so the database never holds anything that could be replayed.
func (s *Store) CreateSession(ctx context.Context, userID, userAgent, ip string) (token string, session Session, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", session, err
	}
	csrf := make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		return "", session, err
	}

	token = base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	session = Session{
		TokenHash: hashToken(token),
		UserID:    userID,
		CSRFToken: base64.RawURLEncoding.EncodeToString(csrf),
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
		UserAgent: truncate(userAgent, 500),
		IP:        ip,
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO admin_sessions (token_hash, user_id, csrf_token, created_at, expires_at, user_agent, ip)
		VALUES (?,?,?,?,?,?,?)`,
		session.TokenHash, session.UserID, session.CSRFToken, timeText(session.CreatedAt),
		timeText(session.ExpiresAt), nullIfEmpty(session.UserAgent), nullIfEmpty(session.IP))
	if err != nil {
		return "", session, err
	}
	return token, session, nil
}

// SessionByToken resolves a cookie to its session and user, treating an expired
// session as absent.
func (s *Store) SessionByToken(ctx context.Context, token string) (Session, AdminUser, error) {
	var (
		session              Session
		createdAt, expiresAt string
		userAgent, ip        sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, csrf_token, created_at, expires_at, user_agent, ip
		FROM admin_sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&session.TokenHash, &session.UserID, &session.CSRFToken, &createdAt, &expiresAt, &userAgent, &ip)
	if errors.Is(err, sql.ErrNoRows) {
		return session, AdminUser{}, ErrNotFound
	}
	if err != nil {
		return session, AdminUser{}, err
	}

	session.CreatedAt, _ = parseTime(createdAt)
	session.ExpiresAt, _ = parseTime(expiresAt)
	session.UserAgent, session.IP = userAgent.String, ip.String

	if time.Now().After(session.ExpiresAt) {
		_ = s.DeleteSession(ctx, token)
		return session, AdminUser{}, ErrNotFound
	}

	user, err := s.AdminByID(ctx, session.UserID)
	if err != nil {
		return session, AdminUser{}, err
	}
	return session, user, nil
}

// TouchSession extends a live session, so someone working in the UI is not
// logged out mid-edit.
func (s *Store) TouchSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE admin_sessions SET expires_at = ? WHERE token_hash = ?`,
		timeText(time.Now().UTC().Add(SessionTTL)), hashToken(token))
	return err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// PurgeExpiredSessions clears out sessions nobody can use any more.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?`, nowText())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
