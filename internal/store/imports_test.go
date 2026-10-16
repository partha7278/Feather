package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestImportRoundTrip(t *testing.T) {
	st, ctx := testStore(t)

	imp := &Import{
		Actor: "ops@example.com", ActorIP: "10.0.0.1",
		SourceKind: ImportSourceArchive, SourceRef: uuid.NewString(), SourceLabel: "orders-00001.ndjson.gz",
		PreferAlias: "cold", TargetAlias: "prod", TargetDatabase: "shop", TargetCollection: "orders_restored_20260101",
		Mode: ImportModeReplace, DryRun: true, FilesTotal: 1, DocsExpected: 100, BytesExpected: 4096,
	}
	if err := st.StartImport(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if imp.ID == uuid.Nil || imp.Status != ImportRunning || imp.StartedAt.IsZero() {
		t.Fatalf("StartImport did not stamp the row: %+v", imp)
	}

	imp.DocsRead, imp.DocsInserted, imp.BytesRead = 40, 40, 1000
	if err := st.UpdateImportProgress(ctx, imp); err != nil {
		t.Fatal(err)
	}
	mid, err := st.ImportByID(ctx, imp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.DocsRead != 40 || mid.FinishedAt != nil || mid.Progress() != 40 {
		t.Errorf("progress not recorded: read %d, finished %v, progress %d", mid.DocsRead, mid.FinishedAt, mid.Progress())
	}

	imp.Status = ImportDryRun
	imp.FilesDone, imp.DocsRead, imp.DocsInserted, imp.DocsPresent, imp.DocsReplaced = 1, 100, 60, 30, 10
	imp.BytesRead, imp.SHA256 = 4096, "abc123"
	if err := st.FinishImport(ctx, imp); err != nil {
		t.Fatal(err)
	}

	got, err := st.ImportByID(ctx, imp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor != imp.Actor || got.ActorIP != imp.ActorIP || got.SourceKind != imp.SourceKind ||
		got.SourceRef != imp.SourceRef || got.SourceLabel != imp.SourceLabel || got.PreferAlias != "cold" ||
		got.TargetAlias != "prod" || got.Target() != "shop.orders_restored_20260101" ||
		got.Mode != ImportModeReplace || !got.DryRun || got.Status != ImportDryRun {
		t.Errorf("descriptive fields did not survive: %+v", got)
	}
	if got.FilesTotal != 1 || got.FilesDone != 1 || got.DocsExpected != 100 || got.BytesExpected != 4096 ||
		got.DocsRead != 100 || got.DocsInserted != 60 || got.DocsPresent != 30 || got.DocsReplaced != 10 ||
		got.BytesRead != 4096 || got.SHA256 != "abc123" {
		t.Errorf("counters did not survive: %+v", got)
	}
	if got.FinishedAt == nil || got.Duration() < 0 || got.Progress() != 100 {
		t.Errorf("finish not recorded: finished %v, progress %d", got.FinishedAt, got.Progress())
	}
}

// Timestamps are compared as text, so they have to be written in the padded
// layout like every other table's (see timeLayout).
func TestImportTimestampsUseThePaddedLayout(t *testing.T) {
	st, ctx := testStore(t)
	imp := &Import{Actor: "a", SourceKind: ImportSourceUpload, TargetAlias: "p", TargetDatabase: "d",
		TargetCollection: "c", Mode: ImportModeInsert}
	if err := st.StartImport(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishImport(ctx, imp); err != nil {
		t.Fatal(err)
	}
	var started, finished string
	if err := st.DB().QueryRowContext(ctx, `SELECT started_at, finished_at FROM feather_imports WHERE id = ?`,
		imp.ID.String()).Scan(&started, &finished); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{started, finished} {
		if len(value) != len("2006-01-02T15:04:05.000000Z") {
			t.Errorf("%q is not in the fixed-width layout", value)
		}
	}
}

func TestImportsAreListedNewestFirst(t *testing.T) {
	st, ctx := testStore(t)

	var ids []uuid.UUID
	for range 3 {
		imp := &Import{Actor: "a", SourceKind: ImportSourceUpload, TargetAlias: "p", TargetDatabase: "d",
			TargetCollection: "c", Mode: ImportModeInsert}
		if err := st.StartImport(ctx, imp); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, imp.ID)
	}
	// The same instant for all three: rowid has to break the tie.
	if _, err := st.DB().ExecContext(ctx, `UPDATE feather_imports SET started_at = ?`, nowText()); err != nil {
		t.Fatal(err)
	}

	listed, err := st.Imports(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d imports, want 3", len(listed))
	}
	for i, want := range []uuid.UUID{ids[2], ids[1], ids[0]} {
		if listed[i].ID != want {
			t.Errorf("position %d is %s, want %s", i, listed[i].ID, want)
		}
	}

	limited, err := st.Imports(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d", len(limited))
	}
}

func TestFailStaleImportsClosesOnlyOpenRows(t *testing.T) {
	st, ctx := testStore(t)

	open := &Import{Actor: "a", SourceKind: ImportSourceUpload, TargetAlias: "p", TargetDatabase: "d",
		TargetCollection: "c", Mode: ImportModeInsert}
	done := &Import{Actor: "a", SourceKind: ImportSourceUpload, TargetAlias: "p", TargetDatabase: "d",
		TargetCollection: "c", Mode: ImportModeInsert}
	for _, imp := range []*Import{open, done} {
		if err := st.StartImport(ctx, imp); err != nil {
			t.Fatal(err)
		}
	}
	done.Status = ImportSuccess
	if err := st.FinishImport(ctx, done); err != nil {
		t.Fatal(err)
	}

	closed, err := st.FailStaleImports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("closed %d rows, want 1", closed)
	}

	got, err := st.ImportByID(ctx, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ImportFailed || got.FinishedAt == nil || got.Error != "interrupted by a restart" {
		t.Errorf("open import not closed properly: %+v", got)
	}
	untouched, err := st.ImportByID(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Status != ImportSuccess || untouched.Error != "" {
		t.Errorf("a finished import was changed: %+v", untouched)
	}
}

func TestImportByIDReportsNotFound(t *testing.T) {
	st, ctx := testStore(t)
	if _, err := st.ImportByID(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestImportProgressFallsBackToBytes(t *testing.T) {
	cases := []struct {
		imp  Import
		want int
	}{
		{Import{}, 0},
		{Import{DocsExpected: 200, DocsRead: 50}, 25},
		{Import{BytesExpected: 1000, BytesRead: 900}, 90},
		{Import{DocsExpected: 10, DocsRead: 12}, 100},
	}
	for _, c := range cases {
		if got := c.imp.Progress(); got != c.want {
			t.Errorf("%+v: progress %d, want %d", c.imp, got, c.want)
		}
	}
}

func TestOnlyAReplaceImportIsDestructive(t *testing.T) {
	if !(AuditEvent{Action: AuditImportReplaceStarted}).Destructive() {
		t.Error("a replace import should be marked destructive")
	}
	for _, action := range []string{AuditImportStarted, AuditImportFinished} {
		if (AuditEvent{Action: action}).Destructive() {
			t.Errorf("%s should not be marked destructive", action)
		}
	}
}
