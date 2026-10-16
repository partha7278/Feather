package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unifers/feather/internal/secrets"
)

func TestMetaFallsBackUntilSetAndThenOverwrites(t *testing.T) {
	st, ctx := testStore(t)

	if got, err := st.Meta(ctx, "retention.audit_days", "90"); err != nil || got != "90" {
		t.Fatalf("an unset key read as %q (%v), want the fallback", got, err)
	}
	if got, err := st.MetaInt(ctx, "retention.audit_days", 90); err != nil || got != 90 {
		t.Fatalf("an unset int read as %d (%v), want the fallback", got, err)
	}

	if err := st.SetMeta(ctx, "retention.audit_days", "30"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta(ctx, "retention.audit_days", "45"); err != nil {
		t.Fatalf("overwriting a key failed: %v", err)
	}
	if got, err := st.Meta(ctx, "retention.audit_days", "90"); err != nil || got != "45" {
		t.Errorf("read back %q (%v), want the latest value", got, err)
	}
	if got, err := st.MetaInt(ctx, "retention.audit_days", 90); err != nil || got != 45 {
		t.Errorf("read back %d (%v), want 45", got, err)
	}
}

func TestMetaIntFallsBackOnValuesThatAreNotNumbers(t *testing.T) {
	st, ctx := testStore(t)

	cases := map[string]int{"": 7, "seven": 7, "1.5": 7, "-3": -3, "12": 12}
	for raw, want := range cases {
		if err := st.SetMeta(ctx, "k", raw); err != nil {
			t.Fatal(err)
		}
		got, err := st.MetaInt(ctx, "k", 7)
		if err != nil {
			t.Errorf("%q gave an error rather than the fallback: %v", raw, err)
		}
		if got != want {
			t.Errorf("%q read as %d, want %d", raw, got, want)
		}
	}
}

// A backup has to be a complete, readable database taken while feather keeps
// running — and it must never quietly replace an earlier one.
func TestBackupWritesAReadableSnapshotAndNeverOverwrites(t *testing.T) {
	st, ctx := testStore(t)
	source, _ := seedAccounts(t, st, ctx)
	if err := st.SetMeta(ctx, "marker", "before-backup"); err != nil {
		t.Fatal(err)
	}

	// A quote in the path exercises the SQL literal escaping VACUUM INTO needs.
	path := filepath.Join(t.TempDir(), "it's here", "nested", "backup.db")
	size, err := st.Backup(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 || size != info.Size() {
		t.Errorf("reported size %d, file is %d", size, info.Size())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the backup holds sealed credentials but is mode %o", perm)
	}

	// Writes after the snapshot must not appear in it.
	if err := st.SetMeta(ctx, "marker", "after-backup"); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(ctx, path, st.secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, _ := restored.Meta(ctx, "marker", ""); got != "before-backup" {
		t.Errorf("the backup holds marker %q, want the value at backup time", got)
	}
	account, err := restored.MongoSourceByID(ctx, source.ID)
	if err != nil {
		t.Fatalf("the backup's credentials cannot be read with the same key: %v", err)
	}
	if account.URI != source.URI {
		t.Errorf("the backup's connection string is %q", account.URI)
	}

	if _, err := st.Backup(ctx, path); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Errorf("a second backup to the same path gave %v", err)
	}
}

func TestBackupRefusesPathsThatWouldDoHarm(t *testing.T) {
	st, ctx := testStore(t)

	for _, path := range []string{"", "   "} {
		if _, err := st.Backup(ctx, path); err == nil {
			t.Errorf("backup to %q was accepted", path)
		}
	}
	if _, err := st.Backup(ctx, st.Path()); err == nil || !strings.Contains(err.Error(), "live database") {
		t.Errorf("backing up over the live database gave %v", err)
	}
	if err := st.Ping(ctx); err != nil {
		t.Errorf("a refused backup damaged the live database: %v", err)
	}
}

func TestPingFailsOnceTheDatabaseIsClosed(t *testing.T) {
	st, ctx := testStore(t)
	if err := st.Ping(ctx); err != nil {
		t.Fatalf("a fresh store does not answer: %v", err)
	}
	st.Close()
	if err := st.Ping(ctx); err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Errorf("a closed store gave %v", err)
	}
}

// A database restored beside the wrong key has to fail at startup, saying
// what is wrong, rather than at the next scheduled run.
func TestCheckMasterKeyCatchesAMismatchedKey(t *testing.T) {
	st, ctx := testStore(t)

	if err := st.CheckMasterKey(ctx); err != nil {
		t.Fatalf("an install with nothing sealed failed the key check: %v", err)
	}
	seedAccounts(t, st, ctx)
	if err := st.CheckMasterKey(ctx); err != nil {
		t.Fatalf("the right key failed the check: %v", err)
	}

	otherKey, err := secrets.GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := secrets.Resolve(otherKey, "")
	if err != nil {
		t.Fatal(err)
	}
	path := st.Path()
	st.Close()

	reopened, err := Open(ctx, path, wrong)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.CheckMasterKey(ctx); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("the wrong key gave %v", err)
	}
}

// Connection test failures are rendered in the UI, and drivers quote the URI
// back — so the stored message must be redacted, not just the displayed one.
func TestRecordedChecksAreRedactedAndClearOnSuccess(t *testing.T) {
	st, ctx := testStore(t)
	source, destination := seedAccounts(t, st, ctx)

	if source.Check.Tested() {
		t.Fatal("a new account reports a check that never ran")
	}

	failure := errors.New("error parsing uri: mongodb://user:hunter2@db.example.com:27017")
	if err := st.RecordMongoCheck(ctx, source.ID, failure); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDestinationCheck(ctx, destination.ID, failure); err != nil {
		t.Fatal(err)
	}

	loadedSource, err := st.MongoSourceByID(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedDest, err := st.DestinationByID(ctx, destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]CheckResult{"mongo": loadedSource.Check, "destination": loadedDest.Check} {
		if !check.Tested() || check.OK {
			t.Errorf("%s: a failed check reads tested=%v ok=%v", name, check.Tested(), check.OK)
		}
		if strings.Contains(check.Error, "hunter2") {
			t.Errorf("%s: the password was stored in the check error: %s", name, check.Error)
		}
		if !strings.Contains(check.Error, "db.example.com:27017") {
			t.Errorf("%s: the useful part of the error was lost: %s", name, check.Error)
		}
	}

	if err := st.RecordMongoCheck(ctx, source.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDestinationCheck(ctx, destination.ID, nil); err != nil {
		t.Fatal(err)
	}
	loadedSource, _ = st.MongoSourceByID(ctx, source.ID)
	loadedDest, _ = st.DestinationByID(ctx, destination.ID)
	for name, check := range map[string]CheckResult{"mongo": loadedSource.Check, "destination": loadedDest.Check} {
		if !check.OK || check.Error != "" || check.At == nil || time.Since(*check.At) > time.Minute {
			t.Errorf("%s: a passing check reads %+v", name, check)
		}
	}
}

func TestLostIsOpenWhileTheLeaseIsHeld(t *testing.T) {
	var none *JobLock
	if none.Lost() != nil {
		t.Error("a nil lock should have no lost channel")
	}

	st, ctx := testStore(t)
	lock, ok, err := st.TryLockJob(ctx, "orders")
	if err != nil || !ok {
		t.Fatalf("could not take the lease: %v %v", ok, err)
	}
	defer lock.Release(ctx)

	select {
	case <-lock.Lost():
		t.Fatal("a freshly taken lease reports itself lost")
	default:
	}
}
