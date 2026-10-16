package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Settings keys for feather's backups of its own database.
const (
	// MetaSelfBackupKeep is how many daily snapshots to keep. Zero turns the
	// automatic backup off.
	MetaSelfBackupKeep = "self_backup_keep"
	// MetaSelfBackupDir is where the snapshots are written. Empty means the
	// default: a backups directory beside the database.
	MetaSelfBackupDir = "self_backup_dir"
)

// MaxSelfBackupKeep bounds the retention count. A year of dailies is already
// more than anyone restores from, and the bound keeps a typo from filling the
// disk.
const MaxSelfBackupKeep = 365

// selfBackupInterval is how often the loop checks whether today's snapshot
// exists. Hourly rather than daily, so a process started at 23:50 or a disk
// that was full for an hour still gets the day's backup.
const selfBackupInterval = time.Hour

const (
	selfBackupPrefix     = "feather-"
	selfBackupSuffix     = ".db"
	selfBackupDateLayout = "2006-01-02"
)

// SelfBackup is one snapshot of feather's database on disk.
type SelfBackup struct {
	Name string
	Path string
	Day  time.Time
	Size int64
}

// SelfBackupSettings is the effective configuration of the automatic backup.
type SelfBackupSettings struct {
	Keep       int
	Dir        string
	DefaultDir string
}

// Enabled reports whether automatic backups are on.
func (s SelfBackupSettings) Enabled() bool { return s.Keep > 0 }

// SelfBackupConfig reads the automatic backup settings.
func (a *App) SelfBackupConfig(ctx context.Context) (SelfBackupSettings, error) {
	keep, err := a.Store.MetaInt(ctx, MetaSelfBackupKeep, 0)
	if err != nil {
		return SelfBackupSettings{}, err
	}
	dir, err := a.Store.Meta(ctx, MetaSelfBackupDir, "")
	if err != nil {
		return SelfBackupSettings{}, err
	}
	settings := SelfBackupSettings{
		Keep:       max(0, min(keep, MaxSelfBackupKeep)),
		DefaultDir: filepath.Join(filepath.Dir(a.Store.Path()), "backups"),
	}
	settings.Dir = strings.TrimSpace(dir)
	if settings.Dir == "" {
		settings.Dir = settings.DefaultDir
	}
	return settings, nil
}

// ListSelfBackups returns the snapshots in dir, newest first. Only files with
// feather's own naming are listed — and only those are ever pruned, so
// pointing the setting at a directory with other files in it is harmless.
func ListSelfBackups(dir string) ([]SelfBackup, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []SelfBackup
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, selfBackupPrefix) || !strings.HasSuffix(name, selfBackupSuffix) {
			continue
		}
		day, err := time.ParseInLocation(selfBackupDateLayout,
			strings.TrimSuffix(strings.TrimPrefix(name, selfBackupPrefix), selfBackupSuffix), time.Local)
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, SelfBackup{Name: name, Path: filepath.Join(dir, name), Day: day, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.After(out[j].Day) })
	return out, nil
}

// RunSelfBackup takes today's snapshot if there is none yet, then prunes down
// to the configured count. One file per calendar day: calling it again the same
// day changes nothing, which is what lets the loop check hourly.
//
// It reports the snapshot it wrote, or nil when today's already existed or
// automatic backups are off.
func (a *App) RunSelfBackup(ctx context.Context) (*SelfBackup, error) {
	a.selfBackupMu.Lock()
	defer a.selfBackupMu.Unlock()

	settings, err := a.SelfBackupConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.Enabled() {
		return nil, nil
	}

	today := time.Now()
	name := selfBackupPrefix + today.Format(selfBackupDateLayout) + selfBackupSuffix
	final := filepath.Join(settings.Dir, name)

	var written *SelfBackup
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		// Written under a temporary name and renamed into place, so a process
		// killed mid-snapshot leaves no file that looks like today's backup —
		// which would otherwise stop the day being retried.
		partial := final + ".partial"
		if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		size, err := a.Store.Backup(ctx, partial)
		if err != nil {
			_ = os.Remove(partial)
			return nil, err
		}
		if err := os.Rename(partial, final); err != nil {
			_ = os.Remove(partial)
			return nil, fmt.Errorf("moving the backup into place: %w", err)
		}
		written = &SelfBackup{Name: name, Path: final, Size: size}
		written.Day, _ = time.ParseInLocation(selfBackupDateLayout, today.Format(selfBackupDateLayout), time.Local)
		a.Log.Info("database backed up", "path", final, "bytes", size)
	} else if err != nil {
		return nil, err
	}

	if err := a.pruneSelfBackups(settings); err != nil {
		return written, err
	}
	return written, nil
}

// pruneSelfBackups removes all but the newest settings.Keep snapshots.
//
// By count, not by age: after a fortnight with feather stopped, an age cut-off
// would delete every backup there is on the first day back.
func (a *App) pruneSelfBackups(settings SelfBackupSettings) error {
	backups, err := ListSelfBackups(settings.Dir)
	if err != nil {
		return err
	}
	if len(backups) <= settings.Keep {
		return nil
	}
	var errs []error
	for _, old := range backups[settings.Keep:] {
		if err := os.Remove(old.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		a.Log.Info("removed old database backup", "path", old.Path)
	}
	return errors.Join(errs...)
}

// StartSelfBackup runs the daily backup loop until ctx is done: once now, then
// on every tick.
func (a *App) StartSelfBackup(ctx context.Context) {
	a.inFlight.Add(1)
	go func() {
		defer a.inFlight.Done()

		check := func() {
			if _, err := a.RunSelfBackup(a.baseCtx); err != nil {
				a.Log.Warn("automatic database backup failed", "error", err)
			}
		}
		check()

		ticker := time.NewTicker(selfBackupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}
