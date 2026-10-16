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
// stolen database should be expensive to attack. It is a variable only so
// UseCheapPasswordHashingForTests can lower it.
var bcryptCost = 12

// UseCheapPasswordHashingForTests drops bcrypt to its minimum cost for the rest
// of the process. Under the race detector a cost-12 hash takes seconds, and a
// package whose every test signs in an administrator then outruns go test's
// timeout. Only a TestMain may call it; nothing outside tests ever should.
func UseCheapPasswordHashingForTests() {
	bcryptCost = bcrypt.MinCost
	hash, err := bcrypt.GenerateFromPassword([]byte("feather-dummy-password"), bcryptCost)
	if err != nil {
		panic(err)
	}
	dummyHash = hash
}

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
	if len(password) > maxPasswordBytes {
		return errors.New("the password must be at most 72 bytes")
	}
	return nil
}

// dummyHash is compared against when there is no real hash to check, so a
// rejection takes as long as a genuine mismatch. Its cost must match bcryptCost.
var dummyHash = []byte("$2a$12$oGCZQPPuY6cVMWk0lyKUJeaGvVeqGmZ.WnGvNPS7lIT/JqZQTgxOe")

// maxPasswordBytes is bcrypt's limit; it ignores everything past it.
const maxPasswordBytes = 72

// comparePassword checks a password against a stored bcrypt hash. bcrypt only
// reads the first 72 bytes, so without the length check any string that
// merely starts with a 72-byte password would match it. No stored password can
// be longer (setting one is refused), so a longer input is always wrong; the
// dummy comparison keeps that rejection from being measurably faster.
func comparePassword(hash, password string) error {
	if len(password) > maxPasswordBytes {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password[:maxPasswordBytes]))
		return bcrypt.ErrMismatchedHashAndPassword
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// Authenticate checks an email and password pair.
func (s *Store) Authenticate(ctx context.Context, email, password string) (AdminUser, error) {
	user, err := s.AdminByEmail(ctx, strings.TrimSpace(strings.ToLower(email)))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Spend the time anyway; a fast rejection tells an attacker the email
			// is unknown.
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			return AdminUser{}, ErrBadCredentials
		}
		return AdminUser{}, err
	}
	if err := comparePassword(user.PasswordHash, password); err != nil {
		return AdminUser{}, ErrBadCredentials
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = ? WHERE id = ?`, nowText(), user.ID); err != nil {
		return user, err
	}
	return user, nil
}

// CheckPassword confirms an administrator's current password without signing
// anything in or out. It guards the few actions that are worse than whatever a
// stolen session could otherwise do, such as handing over the master key.
func (s *Store) CheckPassword(ctx context.Context, userID, password string) error {
	user, err := s.AdminByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := comparePassword(user.PasswordHash, password); err != nil {
		return ErrBadCredentials
	}
	return nil
}

// ChangePassword re-hashes an administrator's password after checking the
// current one, and drops every session that account has open — including,
// deliberately, the one making the request.
//
// The comment here used to say it did that; the code did not. Someone who
// changes their password because they think their session was stolen expects
// the thief to be logged out, and instead the stolen session stayed valid for
// up to twelve hours. Signing the caller out too is the honest version of that
// promise: there is no way to tell "this session" from "the attacker's session"
// after the fact, and re-entering a password just chosen is a small price.
//
// The caller is expected to redirect to the login page afterwards.
func (s *Store) ChangePassword(ctx context.Context, userID, current, next string) error {
	user, err := s.AdminByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := comparePassword(user.PasswordHash, current); err != nil {
		return errors.New("the current password is not correct")
	}
	if err := CheckPasswordStrength(next); err != nil {
		return err
	}
	if current == next {
		return errors.New("the new password is the same as the current one")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcryptCost)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		string(hash), nowText(), userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_users WHERE id = ?`, id); err != nil {
		return err
	}
	// Their open sessions go with them. The guard already refuses a session
	// whose user has vanished, so this is housekeeping rather than a hole —
	// but rows that nothing will ever clean up are their own kind of problem.
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
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
