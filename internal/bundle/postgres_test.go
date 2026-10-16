package bundle

import (
	"context"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

// A PostgreSQL account and a config that reads it travel together: the account
// is sealed under the passphrase, and the config arrives still pointing at it.
func TestAPostgresAccountAndItsConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	from := newInstance(t)
	seedAccounts(t, from)

	const dsn = "postgres://archiver:pg-secret-77@db.internal:5432/shop?sslmode=require"
	account := store.PostgresSourceRecord{Alias: "warehouse", DSN: dsn, Enabled: true, Note: "reporting"}
	if err := from.Store.SavePostgresSource(ctx, &account); err != nil {
		t.Fatal(err)
	}
	enabled := true
	job := store.JobRecord{Config: &config.JobConfig{
		Name:    "orders-pg",
		Enabled: &enabled,
		Source: config.Source{
			Engine: config.EnginePostgres, PostgresSource: "warehouse",
			Database: "sales", Collection: "orders",
			Filter: config.SQLFilterJSON(`created_at < {{now-90d}}`),
		},
		Output:       config.Output{KeyTemplate: "archive/{config}-{runId}-{seq}{ext}"},
		Destinations: []string{"cold"},
		Schedule:     config.Schedule{Every: "1d"},
	}}
	if err := from.Store.SaveJob(ctx, &job); err != nil {
		t.Fatal(err)
	}

	exported, err := Export(ctx, from, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := exported.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "pg-secret-77") {
		t.Fatal("the bundle carries the PostgreSQL password in the clear")
	}

	to := newInstance(t)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	result, err := decoded.Apply(ctx, to, ImportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	if result.PostgresSources.Created != 1 || result.Configs.Created != 1 {
		t.Fatalf("created %v PostgreSQL accounts and %v configs (notes: %v)",
			result.PostgresSources, result.Configs, result.Notes)
	}
	if !strings.Contains(result.Summary(), "PostgreSQL accounts: 1 added") {
		t.Errorf("summary %q does not mention the PostgreSQL account", result.Summary())
	}

	sources, err := to.Store.ListPostgresSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].DSN != dsn || sources[0].Note != "reporting" {
		t.Fatalf("the account did not survive: %+v", sources)
	}
	got, err := to.Store.JobByName(ctx, "orders-pg")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Config.Source.IsPostgres() || got.Config.Source.PostgresSource != "warehouse" {
		t.Fatalf("the config arrived as %+v", got.Config.Source)
	}

	// Importing again adds nothing.
	again, err := decoded.Apply(ctx, to, ImportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	if again.PostgresSources.Created != 0 || again.PostgresSources.Skipped != 1 {
		t.Fatalf("a repeated import gave %v", again.PostgresSources)
	}
}
