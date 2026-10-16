//go:build integration

package pgsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
)

// These tests run against a real PostgreSQL: set FEATHER_TEST_POSTGRES_DSN
// (`make rig-up` starts one on 127.0.0.1:5433). Each test works in a schema of
// its own, dropped afterwards, so they can share a server.

func testDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("FEATHER_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("FEATHER_TEST_REQUIRE_RIG") != "" {
			t.Fatal("FEATHER_TEST_POSTGRES_DSN is not set and FEATHER_TEST_REQUIRE_RIG is")
		}
		t.Skip("FEATHER_TEST_POSTGRES_DSN is not set — run `make rig-up`")
	}
	pool, err := Connect(t.Context(), config.PostgresSource{Alias: "test", DSN: dsn})
	if err != nil {
		t.Fatalf("connecting to %s: %v", config.RedactPostgresDSN(dsn), err)
	}
	t.Cleanup(pool.Close)

	schema := "feather_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	mustExec(t, pool, `CREATE SCHEMA `+schema)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(t.Context()), `DROP SCHEMA `+schema+` CASCADE`)
	})
	return pool, schema
}

func mustExec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, db *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ordersTable creates a table with the column types most likely to lose
// something on the way to JSON and back: microsecond timestamps, a numeric
// wider than a float64, a pretty-printed json value with newlines in it,
// arrays, bytea, a generated column and an identity key.
func ordersTable(t *testing.T, db *pgxpool.Pool, schema string, rows int) {
	t.Helper()
	mustExec(t, db, fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			placed_at  timestamptz NOT NULL,
			total      numeric(30, 10) NOT NULL,
			status     text NOT NULL,
			doc        json,
			attrs      jsonb,
			tags       text[],
			blob       bytea,
			ref        uuid,
			total_x2   numeric GENERATED ALWAYS AS (total * 2) STORED
		)`, schema))
	mustExec(t, db, fmt.Sprintf(`
		INSERT INTO %s.orders (placed_at, total, status, doc, attrs, tags, blob, ref)
		SELECT timestamptz '2025-01-01 00:00:00.123456+00' + (n || ' hours')::interval,
		       12345678901234567890.0123456789 + n,
		       CASE WHEN n %% 3 = 0 THEN 'open' ELSE 'closed' END,
		       ('{' || chr(10) || '  "n": ' || n || ',' || chr(10) || '  "note": "line\nbreak"' || chr(10) || '}')::json,
		       jsonb_build_object('n', n, 'nested', jsonb_build_array(1, 'two', null)),
		       ARRAY['a', 'b''c', NULL],
		       decode('00ff10', 'hex'),
		       gen_random_uuid()
		FROM generate_series(1, %d) AS n`, schema, rows))
}

// snapshot renders every row for comparison. It goes through jsonb, which
// compares a json column by value: the one thing an archive does not keep
// byte for byte is the insignificant whitespace inside a json (not jsonb)
// value, whose newlines become spaces so the row fits on one line.
func snapshot(t *testing.T, db *pgxpool.Pool, schema string) map[int64]string {
	t.Helper()
	rows, err := db.Query(t.Context(), fmt.Sprintf(`SELECT id, to_jsonb(o)::text FROM %s.orders o`, schema))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var line string
		if err := rows.Scan(&id, &line); err != nil {
			t.Fatal(err)
		}
		out[id] = line
	}
	return out
}

func drain(t *testing.T, src *TableSource, fetch int32) ([][]byte, []bson.RawValue, []*time.Time) {
	t.Helper()
	stream, err := src.OpenCursor(t.Context(), fetch)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close(t.Context())
	var (
		lines [][]byte
		ids   []bson.RawValue
		stamp []*time.Time
	)
	for {
		doc, ok, err := stream.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return lines, ids, stamp
		}
		lines = append(lines, doc.Line)
		ids = append(ids, doc.ID)
		stamp = append(stamp, doc.TS)
	}
}

func TestDescribeFindsTheKeyAndRefusesWhatItCannotUse(t *testing.T) {
	db, schema := testDB(t)
	mustExec(t, db, `CREATE TABLE `+schema+`.keyed (code text PRIMARY KEY, v int)`)
	mustExec(t, db, `CREATE TABLE `+schema+`.composite (a int, b int, PRIMARY KEY (a, b))`)
	mustExec(t, db, `CREATE TABLE `+schema+`.keyless (a int)`)

	table, err := Describe(t.Context(), db, schema, "keyed")
	if err != nil {
		t.Fatalf("Describe(keyed): %v", err)
	}
	if table.PrimaryKey() != "code" || table.pkType != "text" {
		t.Errorf("key = %s %s, want code text", table.PrimaryKey(), table.pkType)
	}

	for name, want := range map[string]string{
		"composite": "composite primary key",
		"keyless":   "no primary key",
		"missing":   "does not exist",
	} {
		if _, err := Describe(t.Context(), db, schema, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Describe(%s) = %v, want an error mentioning %q", name, err, want)
		}
	}
}

// The whole cycle: read in pages, delete what was read, restore it from the
// archived lines, and get back rows identical to the ones that left.
func TestRowsSurviveTheRoundTripExactly(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 250)
	before := snapshot(t, db, schema)

	src, err := Bind(t.Context(), db, schema, "orders", `status = 'closed'`, "placed_at")
	if err != nil {
		t.Fatal(err)
	}
	found, err := src.Fence(t.Context())
	if err != nil || !found {
		t.Fatalf("Fence = %v, %v", found, err)
	}
	matching, err := src.Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantClosed := count(t, db, `SELECT count(*) FROM `+schema+`.orders WHERE status = 'closed'`)
	if matching != wantClosed {
		t.Fatalf("Count = %d, want %d", matching, wantClosed)
	}

	// A page size that does not divide the total, so the last page is short.
	lines, ids, stamps := drain(t, src, 37)
	if int64(len(lines)) != wantClosed {
		t.Fatalf("streamed %d rows, want %d", len(lines), wantClosed)
	}
	for i, line := range lines {
		if strings.ContainsAny(string(line), "\n\r") {
			t.Fatalf("row %d spans more than one line: %q", i, line)
		}
		if !json.Valid(line) {
			t.Fatalf("row %d is not JSON: %s", i, line)
		}
		if stamps[i] == nil {
			t.Fatalf("row %d has no timestamp", i)
		}
	}
	// Key order, so a batch describes a contiguous range of keys.
	var previous int64
	for i, id := range ids {
		key, _ := id.StringValueOK()
		var n int64
		fmt.Sscan(key, &n)
		if i > 0 && n <= previous {
			t.Fatalf("keys out of order: %d after %d", n, previous)
		}
		previous = n
	}

	deleted, err := src.DeleteByIDs(t.Context(), discard(), ids, 40)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != wantClosed {
		t.Fatalf("deleted %d rows, want %d", deleted, wantClosed)
	}
	if left := count(t, db, `SELECT count(*) FROM `+schema+`.orders WHERE status = 'closed'`); left != 0 {
		t.Fatalf("%d closed rows survived the delete", left)
	}

	writer, err := NewWriter(t.Context(), db, schema, "orders")
	if err != nil {
		t.Fatal(err)
	}
	inserted, replaced, present, err := writer.WriteRows(t.Context(), lines, false, false)
	if err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if inserted != wantClosed || replaced != 0 || present != 0 {
		t.Fatalf("restore inserted %d, replaced %d, present %d; want %d, 0, 0", inserted, replaced, present, wantClosed)
	}

	after := snapshot(t, db, schema)
	if len(after) != len(before) {
		t.Fatalf("%d rows after the round trip, %d before", len(after), len(before))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("row %d changed on the round trip:\nbefore %s\nafter  %s", id, want, got)
		}
	}

	// Restoring again finds everything already there and changes nothing.
	inserted, _, present, err = writer.WriteRows(t.Context(), lines, false, false)
	if err != nil || inserted != 0 || present != wantClosed {
		t.Fatalf("second restore: inserted %d, present %d, err %v", inserted, present, err)
	}
}

func TestReplaceOverwritesRowsThatChangedSinceTheArchive(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 10)

	src, err := Bind(t.Context(), db, schema, "orders", ``, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fence(t.Context()); err != nil {
		t.Fatal(err)
	}
	lines, _, _ := drain(t, src, 100)

	mustExec(t, db, `UPDATE `+schema+`.orders SET status = 'tampered' WHERE id <= 4`)
	mustExec(t, db, `DELETE FROM `+schema+`.orders WHERE id > 8`)

	writer, err := NewWriter(t.Context(), db, schema, "orders")
	if err != nil {
		t.Fatal(err)
	}
	inserted, replaced, present, err := writer.WriteRows(t.Context(), lines, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 2 || replaced != 8 || present != 0 {
		t.Fatalf("inserted %d, replaced %d, present %d; want 2, 8, 0", inserted, replaced, present)
	}
	if n := count(t, db, `SELECT count(*) FROM `+schema+`.orders WHERE status = 'tampered'`); n != 0 {
		t.Fatalf("%d rows still tampered after a replace", n)
	}
}

// A row inserted after the fence is the next run's, even if it matches.
func TestTheFenceKeepsRowsWrittenMidRunOutOfTheRun(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 30)

	src, err := Bind(t.Context(), db, schema, "orders", ``, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fence(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO `+schema+`.orders (placed_at, total, status) VALUES (now(), 1, 'closed')`)

	n, err := src.Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lines, ids, _ := drain(t, src, 7)
	if n != 30 || len(lines) != 30 {
		t.Fatalf("count %d, streamed %d; want 30 and 30 — the row inserted after the fence leaked in", n, len(lines))
	}

	// And a delete naming a key beyond the fence does not reach it.
	t31 := bson.RawValue{Type: bson.TypeString, Value: mustValue("31")}
	deleted, err := src.DeleteByIDs(t.Context(), discard(), append(ids, t31), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 30 {
		t.Fatalf("deleted %d, want 30", deleted)
	}
	if left := count(t, db, `SELECT count(*) FROM `+schema+`.orders`); left != 1 {
		t.Fatalf("%d rows left, want only the one written after the fence", left)
	}
}

func mustValue(s string) []byte {
	_, v, _ := bson.MarshalValue(s)
	return v
}

// A row that stopped matching between the read and the delete must survive,
// though its id was archived.
func TestADeleteReappliesTheFilter(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 12)

	src, err := Bind(t.Context(), db, schema, "orders", `status = 'closed'`, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fence(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, ids, _ := drain(t, src, 100)

	mustExec(t, db, `UPDATE `+schema+`.orders SET status = 'reopened' WHERE id = 1`)

	deleted, err := src.DeleteByIDs(t.Context(), discard(), ids, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != int64(len(ids))-1 {
		t.Fatalf("deleted %d of %d archived rows, want all but the reopened one", deleted, len(ids))
	}
	if n := count(t, db, `SELECT count(*) FROM `+schema+`.orders WHERE id = 1`); n != 1 {
		t.Fatal("the row that stopped matching was deleted anyway")
	}
}

// Text and uuid keys travel as text and are cast back; a table-qualified
// filter still resolves inside the paging subquery.
func TestNonIntegerKeysAndQualifiedFilters(t *testing.T) {
	db, schema := testDB(t)
	mustExec(t, db, `CREATE TABLE `+schema+`.events (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), kind text, at timestamp)`)
	mustExec(t, db, `INSERT INTO `+schema+`.events (kind, at)
		SELECT CASE WHEN n % 2 = 0 THEN 'old' ELSE 'new' END, timestamp '2025-01-01' + (n || ' days')::interval
		FROM generate_series(1, 41) n`)

	filter, err := config.ResolveSQLTimeTokens(`events.kind = 'old' AND at < {{now}}`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	src, err := Bind(t.Context(), db, schema, "events", filter, "at")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fence(t.Context()); err != nil {
		t.Fatal(err)
	}
	lines, ids, _ := drain(t, src, 6)
	if len(lines) != 20 {
		t.Fatalf("streamed %d rows, want 20", len(lines))
	}
	deleted, err := src.DeleteByIDs(t.Context(), discard(), ids, 7)
	if err != nil || deleted != 20 {
		t.Fatalf("deleted %d, err %v; want 20", deleted, err)
	}
}

func TestAFilterTypoFailsOnceRatherThanBeingRetried(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 3)

	src, err := Bind(t.Context(), db, schema, "orders", `no_such_column = 1`, "")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := src.Fence(t.Context()); err == nil {
		t.Fatal("a filter naming a missing column was accepted")
	}
	if _, err := src.DeleteByIDs(t.Context(), discard(), []bson.RawValue{{Type: bson.TypeString, Value: mustValue("1")}}, 10); err == nil {
		t.Fatal("a delete with a broken filter succeeded")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a permanent SQL error took %s — it was retried", elapsed)
	}
}

func TestBindRefusesAnUnsafeFilterEvenIfItReachesIt(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 3)
	if _, err := Bind(t.Context(), db, schema, "orders", `1=1) OR (1=1`, ""); err == nil {
		t.Fatal("Bind accepted a filter that escapes its parentheses")
	}
}

func TestSchemasAndTablesAreListed(t *testing.T) {
	db, schema := testDB(t)
	ordersTable(t, db, schema, 1)

	schemas, err := Schemas(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(schemas, schema) || contains(schemas, "pg_catalog") || contains(schemas, "information_schema") {
		t.Errorf("Schemas = %v", schemas)
	}
	tables, err := Tables(t.Context(), db, schema)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || tables[0] != "orders" {
		t.Errorf("Tables = %v, want [orders]", tables)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
