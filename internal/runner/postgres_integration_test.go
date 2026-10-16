//go:build integration

package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/pgsrc"
	"github.com/unifers/feather/internal/restore"
	"github.com/unifers/feather/internal/store"
)

// The runner's promise, held against a real PostgreSQL table: rows go only once
// every copy is stored and verified, and what was stored restores to the rows
// that went.

func postgresRig(t *testing.T, rows int) (*rig, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("FEATHER_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("FEATHER_TEST_REQUIRE_RIG") != "" {
			t.Fatal("FEATHER_TEST_POSTGRES_DSN is not set and FEATHER_TEST_REQUIRE_RIG is")
		}
		t.Skip("FEATHER_TEST_POSTGRES_DSN is not set — run `make rig-up`")
	}

	r := newRig(t, 0)
	account := config.PostgresSource{Alias: "warehouse", DSN: dsn}
	pool := pgsrc.NewPool(map[string]config.PostgresSource{"warehouse": account})
	t.Cleanup(pool.Close)
	r.runner.UsePostgres(pool)

	db, err := pool.Get(t.Context(), "warehouse")
	if err != nil {
		t.Fatal(err)
	}
	schema := "feather_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, sql := range []string{
		`CREATE SCHEMA ` + schema,
		`CREATE TABLE ` + schema + `.orders (
			id bigserial PRIMARY KEY, placed_at timestamptz NOT NULL, status text NOT NULL, total numeric(20, 4))`,
		`INSERT INTO ` + schema + `.orders (placed_at, status, total)
			SELECT timestamptz '2024-01-01' + (n || ' hours')::interval,
			       CASE WHEN n % 4 = 0 THEN 'open' ELSE 'closed' END, n * 1.2345
			FROM generate_series(1, ` + strconv.Itoa(rows) + `) n`,
	} {
		if _, err := db.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() { _, _ = db.Exec(context.WithoutCancel(t.Context()), `DROP SCHEMA `+schema+` CASCADE`) })

	r.job.Source = config.Source{
		Engine:         config.EnginePostgres,
		PostgresSource: "warehouse",
		Database:       schema,
		Collection:     "orders",
		Filter:         config.SQLFilterJSON(`status = 'closed' AND placed_at < {{now}}`),
		TimestampField: "placed_at",
	}
	r.job.Batch = config.Batch{MaxDocs: 100, FetchSize: 33}
	if err := r.job.Prepare(); err != nil {
		t.Fatal(err)
	}
	return r, db, schema
}

func countRows(t *testing.T, db *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAPostgresRunArchivesDeletesAndRestores(t *testing.T) {
	r, db, schema := postgresRig(t, 500)
	r.job.Retention.DeleteAfterArchive = true
	table := schema + `.orders`

	closed := countRows(t, db, `SELECT count(*) FROM `+table+` WHERE status = 'closed'`)
	outcome, err := r.runner.Run(t.Context(), r.job, store.TriggerManual)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if outcome.DocsArchived != closed || outcome.DocsDeleted != closed {
		t.Fatalf("archived %d, deleted %d; want %d and %d", outcome.DocsArchived, outcome.DocsDeleted, closed, closed)
	}
	if outcome.Batches != int((closed+99)/100) {
		t.Errorf("%d batches, want %d", outcome.Batches, (closed+99)/100)
	}
	if left := countRows(t, db, `SELECT count(*) FROM `+table+` WHERE status = 'closed'`); left != 0 {
		t.Fatalf("%d closed rows left after the run", left)
	}
	if open := countRows(t, db, `SELECT count(*) FROM `+table+` WHERE status = 'open'`); open != 125 {
		t.Fatalf("%d open rows left, want 125 — the run touched rows outside its filter", open)
	}

	artifacts, err := r.store.ArtifactsForRun(t.Context(), outcome.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := pgsrc.NewWriter(t.Context(), db, schema, "orders")
	if err != nil {
		t.Fatal(err)
	}
	var restored int64
	for _, item := range artifacts {
		if !item.IsPostgres() {
			t.Fatalf("archive %s is catalogued as %q, want postgres", item.FileName, item.Engine)
		}
		if item.MinTS == nil || item.MaxTS == nil {
			t.Errorf("archive %s has no date range", item.FileName)
		}
		stored := r.destinations["primary"].stored()[item.Copies[0].ObjectKey]
		if stored == nil {
			t.Fatalf("archive %s is not in the primary destination", item.FileName)
		}
		result, err := restore.StreamTable(t.Context(), discardLog(), writer, bytes.NewReader(stored), item.Codec, restore.Options{})
		if err != nil {
			t.Fatalf("restoring %s: %v", item.FileName, err)
		}
		if result.SHA256 != item.SHA256 {
			t.Fatalf("archive %s read back as %s, catalogued as %s", item.FileName, result.SHA256, item.SHA256)
		}
		restored += result.Inserted
	}
	if restored != closed {
		t.Fatalf("restored %d rows, want %d", restored, closed)
	}
	if back := countRows(t, db, `SELECT count(*) FROM `+table+` WHERE status = 'closed'`); back != closed {
		t.Fatalf("%d closed rows after restore, want %d", back, closed)
	}
}

// The design's one promise, on the PostgreSQL path: a failed copy deletes
// nothing.
func TestAPostgresRunDeletesNothingWhenACopyFails(t *testing.T) {
	r, db, schema := postgresRig(t, 200)
	r.job.Retention.DeleteAfterArchive = true
	r.destinations["backup"].verifyErr = errors.New("checksum does not match")

	before := countRows(t, db, `SELECT count(*) FROM `+schema+`.orders`)
	if _, err := r.runner.Run(t.Context(), r.job, store.TriggerManual); err == nil {
		t.Fatal("the run succeeded though a copy failed verification")
	}
	if after := countRows(t, db, `SELECT count(*) FROM `+schema+`.orders`); after != before {
		t.Fatalf("%d rows deleted after a copy failed verification", before-after)
	}
}
