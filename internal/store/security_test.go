package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/secrets"
)

// Security properties of the persistence layer: hostile strings stay data,
// credentials never reach the file in the clear, sessions cannot be replayed
// from a stolen copy, and a job lease belongs to exactly one holder.

// hostileStrings are the values an attacker who controls a name, a search box
// or a filter would try. Every one of them must be stored and matched as the
// literal text it is.
var hostileStrings = []string{
	`' OR 1=1--`,
	`" OR ""="`,
	`x'; DROP TABLE feather_artifacts; --`,
	`x"; DROP TABLE audit_events; --`,
	`') UNION SELECT password_hash, 1, 1, 1, 1, 1 FROM admin_users --`,
	`%`,
	`_`,
	`\`,
	"null\x00byte",
	"new\nline",
	`1; ATTACH DATABASE '/tmp/pwn.db' AS pwn; --`,
}

func TestHostileStringsAreMatchedLiterallyByEveryLookup(t *testing.T) {
	st, ctx := testStore(t)

	// Something real to leak, if any lookup were to match too much.
	admin, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	run := recordRun(t, st, ctx, "orders-archive", RunSuccess, false, time.Now().Add(-time.Hour))
	artifact := insertArtifact(t, st, ctx, run, 1, 10)
	insertCopy(t, st, ctx, artifact.ID, "cold", "archive")
	if err := st.RecordAudit(ctx, AuditEvent{Actor: admin.Email, Action: AuditConfigSaved,
		TargetKind: "config", Target: "orders-archive"}); err != nil {
		t.Fatal(err)
	}
	seedAccounts(t, st, ctx)
	seedJob(t, st, ctx, "orders", false)

	for _, hostile := range hostileStrings {
		t.Run(hostile, func(t *testing.T) {
			artifacts, total, err := st.Artifacts(ctx, ArtifactQuery{ConfigName: hostile})
			if err != nil || total != 0 || len(artifacts) != 0 {
				t.Errorf("artifacts by config: %d rows, total %d, err %v", len(artifacts), total, err)
			}
			for _, q := range []ArtifactQuery{{RunID: hostile}, {ArtifactID: hostile}} {
				artifacts, total, err := st.Artifacts(ctx, q)
				if err != nil || total != 0 || len(artifacts) != 0 {
					t.Errorf("artifacts %+v: %d rows, total %d, err %v", q, len(artifacts), total, err)
				}
			}

			for _, q := range []AuditQuery{{Action: hostile}, {TargetKind: hostile}, {Target: hostile}, {Actor: hostile}} {
				events, total, err := st.AuditEvents(ctx, q)
				if err != nil || total != 0 || len(events) != 0 {
					t.Errorf("audit %+v: %d rows, total %d, err %v", q, len(events), total, err)
				}
			}

			if job, err := st.JobByName(ctx, hostile); err == nil || job.Config != nil {
				t.Errorf("job by name found %+v", job)
			}
			if _, err := st.AdminByEmail(ctx, hostile); !errors.Is(err, ErrNotFound) {
				t.Errorf("admin by email gave %v, want not found", err)
			}
			if _, err := st.Authenticate(ctx, hostile, hostile); !errors.Is(err, ErrBadCredentials) {
				t.Errorf("authenticating as %q gave %v", hostile, err)
			}
			if _, _, err := st.SessionByToken(ctx, hostile); !errors.Is(err, ErrNotFound) {
				t.Errorf("session lookup gave %v", err)
			}
			if runs, err := st.RecentRuns(ctx, hostile, 10); err != nil || len(runs) != 0 {
				t.Errorf("recent runs: %d rows, err %v", len(runs), err)
			}
			if got, err := st.Meta(ctx, hostile, "fallback"); err != nil || got != "fallback" {
				t.Errorf("meta lookup gave %q, %v", got, err)
			}
		})
	}

	// The data is still all there: nothing was dropped or rewritten.
	for table, want := range map[string]int{
		"feather_artifacts": 1, "audit_events": 1, "admin_users": 1, "job_configs": 1,
	} {
		var got int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil {
			t.Fatalf("%s is gone: %v", table, err)
		}
		if got != want {
			t.Errorf("%s has %d rows, want %d", table, got, want)
		}
	}
}

func TestHostileStringsAreStoredVerbatimAsData(t *testing.T) {
	st, ctx := testStore(t)

	for _, hostile := range hostileStrings {
		t.Run(hostile, func(t *testing.T) {
			if err := st.SetMeta(ctx, hostile, hostile); err != nil {
				t.Fatal(err)
			}
			if got, err := st.Meta(ctx, hostile, ""); err != nil || got != hostile {
				t.Errorf("meta round trip gave %q, %v", got, err)
			}

			run := recordRun(t, st, ctx, hostile, RunSuccess, false, time.Now())
			summary, err := st.Run(ctx, run.ID)
			if err != nil || summary.ConfigName != hostile {
				t.Errorf("run round trip gave %q, %v", summary.ConfigName, err)
			}
			runs, err := st.RecentRuns(ctx, hostile, 10)
			if err != nil || len(runs) != 1 {
				t.Errorf("recent runs for the hostile config: %d, %v", len(runs), err)
			}

			lock, ok, err := st.TryLockJob(ctx, hostile)
			if err != nil || !ok {
				t.Fatalf("leasing a config named %q: %v %v", hostile, ok, err)
			}
			lock.Release(ctx)
		})
	}
}

// The search box is a substring search. % and _ are LIKE metacharacters; typed
// into the box they must mean the characters themselves, not "anything".
func TestArtifactSearchTreatsLikeWildcardsLiterally(t *testing.T) {
	st, ctx := testStore(t)
	run := recordRun(t, st, ctx, "orders", RunSuccess, false, time.Now())

	for i, name := range []string{"plain-00001.ndjson.gz", "under_score.ndjson.gz", "hundred%.ndjson.gz"} {
		a := &Artifact{RunID: run.ID, ConfigName: "orders", Database: "shop", Collection: "orders",
			BatchSeq: i + 1, FileName: name, Format: "ndjson", Codec: "gzip", SHA256: "x"}
		if err := st.InsertArtifact(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		search string
		want   string
	}{
		{"%", "hundred%.ndjson.gz"},
		{"_", "under_score.ndjson.gz"},
		{"d_r", ""}, // "under" matches only if _ is a wildcard; "d_r" is not a substring of any name
	}
	for _, tc := range cases {
		found, total, err := st.Artifacts(ctx, ArtifactQuery{Search: tc.search})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, a := range found {
			names = append(names, a.FileName)
		}
		wantCount := 0
		if tc.want != "" {
			wantCount = 1
		}
		if total != int64(wantCount) || len(found) != wantCount || (wantCount == 1 && names[0] != tc.want) {
			t.Errorf("searching %q matched %v (total %d), want only %q", tc.search, names, total, tc.want)
		}
	}
}

// --- passwords ------------------------------------------------------------

func TestPasswordsAreStoredAsSaltedBcryptAtTheConfiguredCost(t *testing.T) {
	st, ctx := testStore(t)
	const password = "correct-horse-battery"

	first, err := st.CreateAdmin(ctx, "one@example.com", password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateAdmin(ctx, "two@example.com", password)
	if err != nil {
		t.Fatal(err)
	}

	if first.PasswordHash == second.PasswordHash {
		t.Fatal("the same password hashed to the same value twice: there is no salt")
	}
	for _, hash := range []string{first.PasswordHash, second.PasswordHash} {
		cost, err := bcrypt.Cost([]byte(hash))
		if err != nil {
			t.Fatalf("the stored hash is not bcrypt: %v", err)
		}
		if cost < bcryptCost || cost < bcrypt.DefaultCost {
			t.Errorf("bcrypt cost is %d, want at least %d", cost, bcryptCost)
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			t.Error("the stored hash does not verify its own password")
		}
	}
}

func TestPasswordsBeyondBcryptsLimitAreRefusedEverywhere(t *testing.T) {
	st, ctx := testStore(t)

	exactly72 := strings.Repeat("a", 72)
	tooLong := exactly72 + "b"
	// 24 three-byte runes: 24 characters, 72 bytes — and one more is over.
	multibyte := strings.Repeat("€", 24)
	multibyteTooLong := multibyte + "€"

	if err := CheckPasswordStrength(exactly72); err != nil {
		t.Errorf("a 72-byte password was refused: %v", err)
	}
	if err := CheckPasswordStrength(multibyte); err != nil {
		t.Errorf("a 72-byte multibyte password was refused: %v", err)
	}
	for _, long := range []string{tooLong, multibyteTooLong} {
		if err := CheckPasswordStrength(long); err == nil {
			t.Errorf("a %d-byte password was accepted; bcrypt would silently ignore the tail", len(long))
		}
		if _, err := st.CreateAdmin(ctx, "long@example.com", long); err == nil {
			t.Errorf("CreateAdmin accepted a %d-byte password", len(long))
		}
	}

	admin, err := st.CreateAdmin(ctx, "ops@example.com", exactly72)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ChangePassword(ctx, admin.ID, exactly72, tooLong); err == nil {
		t.Error("ChangePassword accepted a password bcrypt would truncate")
	}
	if err := st.ResetPassword(ctx, admin.Email, tooLong); err == nil {
		t.Error("ResetPassword accepted a password bcrypt would truncate")
	}
}

// A user who chose a 72-byte password must not be signed in by a longer string
// that merely starts with it: bcrypt compares only the first 72 bytes.
func TestALongerPasswordSharingTheFirst72BytesDoesNotSignIn(t *testing.T) {
	st, ctx := testStore(t)
	exactly72 := strings.Repeat("k", 72)
	if _, err := st.CreateAdmin(ctx, "ops@example.com", exactly72); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", exactly72+"-anything"); err == nil {
		t.Error("a password longer than the stored one authenticated: bcrypt truncation is reachable at sign-in")
	}
}

func TestNoSecretReachesTheDatabaseFilesInTheClear(t *testing.T) {
	ctx := context.Background()
	key, err := secrets.GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "feather.db")
	st, err := Open(ctx, path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	plaintexts := map[string]string{
		"admin password":      "first-Passphrase-7c1d",
		"changed password":    "second-Passphrase-9e4a",
		"mongo password":      "MongoPw-5f2b8c",
		"s3 secret key":       "S3Secret-a8d0e1f2",
		"s3 session token":    "S3Session-77c3b0",
		"service account key": "PRIVATE-KEY-0d9e3c",
		"oauth client secret": "OAuthClient-6b1a",
		"oauth refresh token": "RefreshTok-2c4e",
		"oci customer secret": "OciSecret-31f9",
	}

	admin, err := st.CreateAdmin(ctx, "ops@example.com", plaintexts["admin password"])
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ChangePassword(ctx, admin.ID, plaintexts["admin password"], plaintexts["changed password"]); err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(ctx, admin.ID, "curl/8", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	plaintexts["session cookie"] = token

	if err := st.SaveMongoSource(ctx, &MongoSourceRecord{Alias: "prod", Enabled: true,
		URI: "mongodb://user:" + plaintexts["mongo password"] + "@db.example.com:27017"}); err != nil {
		t.Fatal(err)
	}
	records := []DestinationRecord{
		{Enabled: true, Destination: config.Destination{Alias: "s3", Provider: config.ProviderS3,
			Region: "ap-south-1", Bucket: "archive", AccessKeyID: "AKIA",
			SecretAccessKey: plaintexts["s3 secret key"], SessionToken: plaintexts["s3 session token"]}},
		{Enabled: true, Destination: config.Destination{Alias: "sa", Provider: config.ProviderGDrive,
			DriveFolderID: "1Abc", ServiceAccountJSON: `{"type":"service_account","client_email":"x@example.iam.gserviceaccount.com","private_key":"` + plaintexts["service account key"] + `"}`}},
		{Enabled: true, Destination: config.Destination{Alias: "drive", Provider: config.ProviderGDrive,
			DriveFolderID: "1Def", OAuthClientID: "id.apps.googleusercontent.com",
			OAuthClientSecret: plaintexts["oauth client secret"], OAuthRefreshToken: plaintexts["oauth refresh token"]}},
		{Enabled: true, Destination: config.Destination{Alias: "oci", Provider: config.ProviderOCI,
			Region: "ap-mumbai-1", Bucket: "cold", Endpoint: "https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com",
			AccessKeyID: "key", SecretAccessKey: plaintexts["oci customer secret"]}},
	}
	for i := range records {
		if err := st.SaveDestination(ctx, &records[i]); err != nil {
			t.Fatalf("saving %s: %v", records[i].Alias, err)
		}
	}

	// Read the files while the store is open, so the WAL still holds whatever
	// has not been checkpointed, then again after a close has checkpointed it.
	scan := func(when string) {
		for _, name := range []string{path, path + "-wal", path + "-shm"} {
			raw, err := os.ReadFile(name)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for what, secret := range plaintexts {
				if bytes.Contains(raw, []byte(secret)) {
					t.Errorf("%s: the %s is readable in %s", when, what, filepath.Base(name))
				}
			}
		}
	}
	scan("while open")
	st.Close()
	scan("after close")
}

// --- sessions -------------------------------------------------------------

func TestSessionTokensAreUnpredictableAndOnlyTheirHashIsStored(t *testing.T) {
	st, ctx := testStore(t)
	admin, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for range 64 {
		token, session, err := st.CreateSession(ctx, admin.ID, "curl/8", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("the token is not base64url: %v", err)
		}
		if len(raw) < 32 {
			t.Fatalf("the session token carries %d bytes of randomness, want at least 32", len(raw))
		}
		csrf, err := base64.RawURLEncoding.DecodeString(session.CSRFToken)
		if err != nil || len(csrf) < 32 {
			t.Fatalf("the CSRF token carries %d bytes (%v), want at least 32", len(csrf), err)
		}
		if session.CSRFToken == token {
			t.Fatal("the CSRF token is the session token")
		}
		if seen[token] || seen[session.CSRFToken] {
			t.Fatal("a token repeated")
		}
		seen[token], seen[session.CSRFToken] = true, true

		digest := sha256.Sum256([]byte(token))
		var stored int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM admin_sessions WHERE token_hash = ?`, hex.EncodeToString(digest[:])).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != 1 {
			t.Fatal("the session is not stored under the SHA-256 of its token")
		}
		var raws int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM admin_sessions WHERE token_hash = ?`, token).Scan(&raws); err != nil {
			t.Fatal(err)
		}
		if raws != 0 {
			t.Fatal("the raw token is stored")
		}
	}
}

// Someone holding a copy of the database has the hashes. A hash presented as a
// cookie must not resolve to a session.
func TestAStolenTokenHashCannotBeReplayedAsACookie(t *testing.T) {
	st, ctx := testStore(t)
	admin, err := st.CreateAdmin(ctx, "ops@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateSession(ctx, admin.ID, "curl/8", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	var hash string
	if err := st.db.QueryRowContext(ctx, `SELECT token_hash FROM admin_sessions`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{hash, strings.ToUpper(hash), "", " "} {
		if _, _, err := st.SessionByToken(ctx, candidate); !errors.Is(err, ErrNotFound) {
			t.Errorf("presenting %q as a cookie gave %v, want not found", candidate, err)
		}
	}
}

// A deleted administrator's cookie must stop working at once, even if the row
// cleanup were ever skipped.
func TestASessionDoesNotOutliveItsAdministrator(t *testing.T) {
	st, ctx := testStore(t)
	if _, err := st.CreateAdmin(ctx, "keep@example.com", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	gone, err := st.CreateAdmin(ctx, "gone@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(ctx, gone.ID, "curl/8", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAdmin(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, user, err := st.SessionByToken(ctx, token); err == nil {
		t.Errorf("a deleted administrator's session still resolves to %s", user.Email)
	}
}

// --- audit ----------------------------------------------------------------

// The audit trail has one way in and one bulk way out (age-based purge). There
// must be no API that rewrites or removes an individual event.
func TestTheAuditTrailHasNoEditOrSingleDeletePath(t *testing.T) {
	allowed := map[string]bool{"RecordAudit": true, "AuditEvents": true, "PurgeAuditBefore": true}
	storeType := reflect.TypeOf(&Store{})
	for i := range storeType.NumMethod() {
		name := storeType.Method(i).Name
		if strings.Contains(name, "Audit") && !allowed[name] {
			t.Errorf("Store.%s touches the audit trail; review whether it can edit or delete events", name)
		}
	}
}

// An actor or target carrying newlines, carriage returns or NULs is stored as
// one event with those bytes intact, so it cannot pose as a second, forged row
// and the renderer sees exactly what was submitted.
func TestAuditFieldsWithControlCharactersStayOneVerbatimEvent(t *testing.T) {
	st, ctx := testStore(t)

	forged := "ops@example.com\n2026-01-01T00:00:00Z admin@example.com config.deletion.disabled"
	event := AuditEvent{
		Actor:      forged,
		ActorIP:    "10.0.0.1\r\nX-Forwarded-For: 1.2.3.4",
		Action:     AuditConfigSaved,
		TargetKind: "config",
		Target:     "orders\x00hidden",
		Detail:     "line one\nline two\u2028\u202eevil",
	}
	if err := st.RecordAudit(ctx, event); err != nil {
		t.Fatal(err)
	}

	events, total, err := st.AuditEvents(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("one recorded event became %d", total)
	}
	got := events[0]
	if got.Actor != event.Actor || got.ActorIP != event.ActorIP || got.Target != event.Target || got.Detail != event.Detail {
		t.Errorf("fields were altered in storage: %+v", got)
	}
	// Filtering on the forged actor's second line must not find it.
	if _, n, err := st.AuditEvents(ctx, AuditQuery{Actor: "admin@example.com"}); err != nil || n != 0 {
		t.Errorf("a substring of a forged actor matched as an actor: %d, %v", n, err)
	}
}

// --- the database file ----------------------------------------------------

func TestTheDatabaseAndItsSidecarsAreOwnerOnly(t *testing.T) {
	st, ctx := testStore(t)
	// Writes, so the WAL and shared-memory files certainly exist.
	seedAccounts(t, st, ctx)
	if err := st.SetMeta(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{st.Path(), st.Path() + "-wal", st.Path() + "-shm"} {
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is mode %o; group or others can read it", filepath.Base(name), perm)
		}
	}
}

func TestADatabaseDirectoryItCreatesIsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	key, err := secrets.GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state", "feather")
	st, err := Open(ctx, filepath.Join(dir, "feather.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the database directory is mode %o", perm)
	}
}

// --- leases ---------------------------------------------------------------

// openSecond opens another Store on the same file, standing in for a second
// feather process pointed at the same database.
func openSecond(t *testing.T, first *Store) *Store {
	t.Helper()
	second, err := Open(context.Background(), first.Path(), first.secrets)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	return second
}

func TestAnotherProcessCannotTakeOrReleaseALiveLease(t *testing.T) {
	st, ctx := testStore(t)
	other := openSecond(t, st)

	mine, ok, err := st.TryLockJob(ctx, "orders")
	if err != nil || !ok {
		t.Fatalf("taking the lease: %v %v", ok, err)
	}
	defer mine.Release(ctx)

	if _, ok, err := other.TryLockJob(ctx, "orders"); err != nil || ok {
		t.Fatalf("a second process took a live lease: ok=%v err=%v", ok, err)
	}
	// Its stale-lease sweep, under a different host name, must not clear it.
	if _, err := other.ClearStaleLeases(ctx, "some-other-host"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := other.TryLockJob(ctx, "orders"); ok {
		t.Fatal("the lease was taken after another process swept for stale leases")
	}
}

func TestAnExpiredLeaseIsTakenOverAndTheOldHolderCannotDeleteIt(t *testing.T) {
	st, ctx := testStore(t)
	other := openSecond(t, st)

	old, ok, err := st.TryLockJob(ctx, "orders")
	if err != nil || !ok {
		t.Fatalf("taking the lease: %v %v", ok, err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE job_leases SET expires_at = ? WHERE config_name = ?`,
		timeText(time.Now().Add(-time.Minute)), "orders"); err != nil {
		t.Fatal(err)
	}

	taken, ok, err := other.TryLockJob(ctx, "orders")
	if err != nil || !ok {
		t.Fatalf("an expired lease was not taken over: %v %v", ok, err)
	}
	defer taken.Release(ctx)

	// The old holder finishing late must not delete the new holder's claim.
	old.Release(ctx)

	var holders int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_leases WHERE config_name = ?`, "orders").Scan(&holders); err != nil {
		t.Fatal(err)
	}
	if holders != 1 {
		t.Fatal("the previous holder's release deleted the new holder's lease")
	}
	if _, ok, _ := st.TryLockJob(ctx, "orders"); ok {
		t.Fatal("the lease was handed out a second time while the new holder has it")
	}
}

func TestConcurrentLockAttemptsFromTwoProcessesYieldOneHolder(t *testing.T) {
	st, ctx := testStore(t)
	stores := []*Store{st, openSecond(t, st)}

	const attempts = 32
	var (
		wg      sync.WaitGroup
		winners atomic.Int64
		mu      sync.Mutex
		held    []*JobLock
		errs    []error
	)
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lock, ok, err := stores[i%2].TryLockJob(ctx, "orders")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if ok {
				winners.Add(1)
				held = append(held, lock)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("a lock attempt failed outright: %v", err)
	}
	if got := winners.Load(); got != 1 {
		t.Errorf("%d concurrent attempts acquired the lease, want exactly 1", got)
	}
	for _, lock := range held {
		lock.Release(ctx)
	}
}

// --- credential previews --------------------------------------------------

// The preview column is unencrypted and shown in the UI. Whatever a connection
// string carries as a credential must not survive into it.
func TestMongoPreviewCarriesNoCredential(t *testing.T) {
	st, ctx := testStore(t)

	cases := []struct {
		name, uri, secret string
	}{
		{"userinfo password", "mongodb://user:pw-1a2b3c@db.example.com:27017/admin", "pw-1a2b3c"},
		{"srv record", "mongodb+srv://user:pw-4d5e6f@cluster0.example.net/?retryWrites=true", "pw-4d5e6f"},
		{"replica set hosts", "mongodb://user:pw-7a8b9c@h1:27017,h2:27017,h3:27017/?replicaSet=rs0", "pw-7a8b9c"},
		{"escaped at sign", "mongodb://user:p%40ss-0d1e@db.example.com", "p%40ss-0d1e"},
		{"raw at sign", "mongodb://user:p@ss-2f3a@db.example.com", "ss-2f3a"},
		{"aws session token", "mongodb://AKIAEXAMPLE:awsSecret-9z8y@db.example.com/?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:sessTok-6c5b", "sessTok-6c5b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := MongoSourceRecord{Alias: "src-" + uuid.NewString()[:8], URI: tc.uri, Enabled: true}
			if err := st.SaveMongoSource(ctx, &record); err != nil {
				t.Fatal(err)
			}
			var preview string
			if err := st.db.QueryRowContext(ctx,
				`SELECT uri_preview FROM mongo_sources WHERE id = ?`, record.ID).Scan(&preview); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(preview, tc.secret) {
				t.Errorf("the stored preview %q contains the credential %q", preview, tc.secret)
			}
		})
	}
}
