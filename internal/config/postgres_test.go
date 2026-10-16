package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSQLFiltersThatStayInsideTheirParenthesesAreAccepted(t *testing.T) {
	for _, filter := range []string{
		``,
		`created_at < {{now-90d}}`,
		`created_at < '{{now-90d}}' AND status = 'closed'`,
		`(status = 'a' OR status = 'b') AND id IN (SELECT order_id FROM refunds)`,
		`note = 'it''s fine; really -- not a comment /* nor this */ ) ('`,
		`"Weird Column;--" = 1`,
		`orders.created_at < now() - interval '90 days'`,
		`path LIKE 'C:\temp\%'`,
	} {
		if err := LintSQLFilter(filter); err != nil {
			t.Errorf("LintSQLFilter(%q) = %v, want nil", filter, err)
		}
	}
}

// The filter is spliced into a DELETE as `... AND (<filter>)`. Every case here
// would, spliced in, either escape the parentheses — widening the delete past
// the archived rows — or hide the conditions after it.
func TestSQLFiltersThatCouldWidenADeleteAreRefused(t *testing.T) {
	for _, filter := range []string{
		`1=1) OR (1=1`,
		`true) OR true --`,
		`id > 0 -- the rest is a comment`,
		`id > 0 /* hidden */`,
		`id > 0; DELETE FROM orders`,
		`(id > 0`,
		`id > 0)`,
		`name = 'unterminated`,
		`"unterminated = 1`,
		`body = $$x$$`,
		`id = $1`,
		`note = E'\') OR (true'`,
		`id > 0 \`,
		`created_at < {{now-90d`,
	} {
		if err := LintSQLFilter(filter); err == nil {
			t.Errorf("LintSQLFilter(%q) = nil, want a refusal", filter)
		}
	}
}

func TestSQLTimeTokensBecomeTypedTimestamps(t *testing.T) {
	now := time.Date(2026, 5, 14, 2, 30, 0, 0, time.UTC)

	cases := map[string]string{
		`created_at < {{now-90d}}`:         `created_at < TIMESTAMPTZ '2026-02-13T02:30:00.000Z'`,
		`created_at < '{{now-90d}}'`:       `created_at < '2026-02-13T02:30:00.000Z'`,
		`a < {{ now }} AND b > {{now+1h}}`: `a < TIMESTAMPTZ '2026-05-14T02:30:00.000Z' AND b > TIMESTAMPTZ '2026-05-14T03:30:00.000Z'`,
		`  status = 'x'  `:                 `status = 'x'`,
	}
	for in, want := range cases {
		got, err := ResolveSQLTimeTokens(in, now)
		if err != nil {
			t.Errorf("ResolveSQLTimeTokens(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveSQLTimeTokens(%q)\n got %q\nwant %q", in, got, want)
		}
	}

	for _, bad := range []string{`created_at < {{niw-90d}}`, `created_at < {{now-90x}}`} {
		if _, err := ResolveSQLTimeTokens(bad, now); err == nil {
			t.Errorf("ResolveSQLTimeTokens(%q) = nil error, want one", bad)
		}
	}
}

func postgresJob() *JobConfig {
	return &JobConfig{
		Name: "orders-pg",
		Source: Source{
			Engine:         EnginePostgres,
			PostgresSource: "warehouse",
			Collection:     "orders",
			Filter:         SQLFilterJSON(`created_at < {{now-90d}}`),
		},
		Destinations: []string{"cold"},
		Schedule:     Schedule{Every: "1d"},
		Output:       Output{KeyTemplate: DefaultKeyTemplate},
	}
}

func TestAPostgresConfigValidates(t *testing.T) {
	job := postgresJob()
	err := job.Validate(Catalog{
		PostgresSources: map[string]PostgresSource{"warehouse": {Alias: "warehouse"}},
		Destinations:    map[string]Destination{"cold": {Alias: "cold"}},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if job.Source.Database != "public" {
		t.Errorf("schema defaulted to %q, want public", job.Source.Database)
	}
	if got := job.Source.Account(); got != "warehouse" {
		t.Errorf("Account() = %q, want warehouse", got)
	}
}

func TestAPostgresConfigIsCheckedAgainstPostgresAccounts(t *testing.T) {
	job := postgresJob()
	err := job.Validate(Catalog{
		// A MongoDB account of the same name is not a PostgreSQL one.
		Sources:         map[string]MongoSource{"warehouse": {Alias: "warehouse"}},
		PostgresSources: map[string]PostgresSource{},
		Destinations:    map[string]Destination{"cold": {Alias: "cold"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not a configured PostgreSQL account") {
		t.Fatalf("Validate = %v, want an unknown PostgreSQL account error", err)
	}
}

func TestPostgresConfigRefusals(t *testing.T) {
	cases := map[string]func(*JobConfig){
		"unsafe filter":   func(j *JobConfig) { j.Source.Filter = SQLFilterJSON(`1=1) OR (1=1`) },
		"filter not text": func(j *JobConfig) { j.Source.Filter = json.RawMessage(`{"a":1}`) },
		"projection":      func(j *JobConfig) { j.Source.Projection = json.RawMessage(`{"a":1}`) },
		"no table":        func(j *JobConfig) { j.Source.Collection = "" },
		"no account":      func(j *JobConfig) { j.Source.PostgresSource = "" },
		"empty filter deletes the table": func(j *JobConfig) {
			j.Source.Filter = SQLFilterJSON("")
			j.Retention.DeleteAfterArchive = true
		},
		"unknown engine": func(j *JobConfig) { j.Source.Engine = "oracle" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			job := postgresJob()
			mutate(job)
			if err := job.Validate(Catalog{}); err == nil {
				t.Fatal("Validate = nil, want a refusal")
			}
		})
	}
}

// Every config stored before PostgreSQL existed has runs recorded against its
// fingerprint. Adding the engine must not change it.
func TestAMongoConfigsFingerprintDoesNotMentionTheEngine(t *testing.T) {
	src := Source{MongoSource: "m", Database: "d", Collection: "c"}
	encoded, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"engine", "postgresSource"} {
		if strings.Contains(string(encoded), field) {
			t.Errorf("a MongoDB source encodes %q: %s", field, encoded)
		}
	}
}

func TestPostgresDSNRedaction(t *testing.T) {
	cases := map[string]string{
		"postgres://app:s3cret@db:5432/shop?sslmode=require": "s3cret",
		"postgres://app@db/shop?password=s3cret":             "s3cret",
		"host=db user=app password=s3cret dbname=shop":       "s3cret",
		"host=db user=app password='s3 cret' dbname=shop":    "s3 cret",
	}
	for dsn, secret := range cases {
		got := RedactPostgresDSN(dsn)
		if strings.Contains(got, secret) {
			t.Errorf("RedactPostgresDSN(%q) = %q, still holds the password", dsn, got)
		}
		if !strings.Contains(got, "db") {
			t.Errorf("RedactPostgresDSN(%q) = %q, lost the host", dsn, got)
		}
	}
}

func TestAPostgresAccountDoesNotPrintItsPassword(t *testing.T) {
	src := PostgresSource{Alias: "warehouse", DSN: "postgres://app:s3cret@db:5432/shop"}
	encoded, _ := json.Marshal(src)
	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", src),
		"%+v":  fmt.Sprintf("%+v", src),
		"%#v":  fmt.Sprintf("%#v", src),
		"json": string(encoded),
	} {
		if strings.Contains(out, "s3cret") {
			t.Errorf("%s exposes the password: %s", name, out)
		}
	}
}
