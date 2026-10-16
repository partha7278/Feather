package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Off by default: nothing is written until a count is set.
func TestSelfBackupIsOffUntilACountIsSet(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()

	written, err := a.RunSelfBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if written != nil {
		t.Fatalf("wrote %s with backups off", written.Path)
	}
	settings, err := a.SelfBackupConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings.Dir); !os.IsNotExist(err) {
		t.Errorf("backup directory exists with backups off: %v", err)
	}
}

// One snapshot a day, however often the check runs.
func TestSelfBackupWritesOncePerDay(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "db-backups")
	setSelfBackup(t, a, 10, dir)

	first, err := a.RunSelfBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("no backup written")
	}
	want := "feather-" + time.Now().Format("2006-01-02") + ".db"
	if first.Name != want || first.Size == 0 {
		t.Errorf("wrote %s (%d bytes), want %s with content", first.Name, first.Size, want)
	}

	second, err := a.RunSelfBackup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Errorf("a second backup was written the same day: %s", second.Path)
	}
	if backups, _ := ListSelfBackups(dir); len(backups) != 1 {
		t.Errorf("%d backups on disk, want 1", len(backups))
	}
}

// Keeping N leaves the newest N days and touches nothing that is not a backup.
func TestSelfBackupKeepsTheNewestN(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	setSelfBackup(t, a, 3, dir)

	now := time.Now()
	for days := 1; days <= 6; days++ {
		name := "feather-" + now.AddDate(0, 0, -days).Format("2006-01-02") + ".db"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := a.RunSelfBackup(ctx); err != nil {
		t.Fatal(err)
	}

	backups, err := ListSelfBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 3 {
		t.Fatalf("%d backups kept, want 3", len(backups))
	}
	for i, b := range backups {
		want := "feather-" + now.AddDate(0, 0, -i).Format("2006-01-02") + ".db"
		if b.Name != want {
			t.Errorf("backup %d is %s, want %s", i, b.Name, want)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("an unrelated file was removed: %v", err)
	}
}

func setSelfBackup(t *testing.T, a *App, keep int, dir string) {
	t.Helper()
	ctx := context.Background()
	if err := a.Store.SetMeta(ctx, MetaSelfBackupKeep, strconv.Itoa(keep)); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetMeta(ctx, MetaSelfBackupDir, dir); err != nil {
		t.Fatal(err)
	}
}
