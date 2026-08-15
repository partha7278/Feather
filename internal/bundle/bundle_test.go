package bundle

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

const passphrase = "correct-horse-battery-staple"

// The whole point: what comes out of one installation goes into another and
// works — credentials included, without the master key travelling with it.
func TestRoundTripBetweenTwoInstallations(t *testing.T) {
	ctx := context.Background()
	source := newInstance(t)
	seed(t, source)

	exported, err := Export(ctx, source, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := exported.Encode()
	if err != nil {
		t.Fatal(err)
	}

	target := newInstance(t)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	result, err := decoded.Apply(ctx, target, ImportOptions{Passphrase: passphrase, Settings: true})
	if err != nil {
		t.Fatalf("%v (notes: %v)", err, result)
	}
	if len(result.Notes) > 0 {
		t.Fatalf("a clean import reported leftovers: %v", result.Notes)
	}
	if result.MongoSources.Created != 1 || result.Destinations.Created != 2 || result.Configs.Created != 1 {
		t.Fatalf("created %v / %v / %v", result.MongoSources, result.Destinations, result.Configs)
	}

	sources, err := target.Store.ListMongoSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].URI != "mongodb://user:hunter2@127.0.0.1:27017" {
		t.Fatalf("the connection string did not survive: %+v", sources)
	}

	destinations, err := target.Store.ListDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byAlias := map[string]store.DestinationRecord{}
	for _, d := range destinations {
		byAlias[d.Alias] = d
	}
	if got := byAlias["cold"].SecretAccessKey; got != "very-secret-key" {
		t.Errorf("the S3 secret came back as %q", got)
	}
	if got := byAlias["drive"].ServiceAccountJSON; !strings.Contains(got, "private-key-material") {
		t.Errorf("the service account key came back as %q", got)
	}

	job, err := target.Store.JobByName(ctx, "orders-archive")
	if err != nil {
		t.Fatal(err)
	}
	if job.Config.Source.Database != "shop" || len(job.Config.Destinations) != 2 {
		t.Errorf("the config did not survive: %+v", job.Config)
	}
	// maxRuntime marshals as a duration string; a number would fail to read back
	// and leave the default in its place.
	if job.Config.Limits.MaxRuntime.Std().String() != "2h0m0s" {
		t.Errorf("maxRuntime came back as %s", job.Config.Limits.MaxRuntime.Std())
	}
	if len(job.Config.Source.Projection) != 0 {
		t.Errorf("an absent projection came back as %q", job.Config.Source.Projection)
	}

	name, err := target.Store.Meta(ctx, app.MetaInstanceName, "")
	if err != nil || name != "production" {
		t.Errorf("the instance name came back as %q (%v)", name, err)
	}
}

// A bundle is meant to leave the machine. Nothing in it may be readable.
func TestExportedFileCarriesNoPlaintextCredentials(t *testing.T) {
	ctx := context.Background()
	instance := newInstance(t)
	seed(t, instance)

	exported, err := Export(ctx, instance, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := exported.Encode()
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"hunter2", "very-secret-key", "private-key-material", "refresh-token"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("%q appears in the exported bundle in the clear", secret)
		}
	}
	// The redacted preview is what makes the file readable to a person.
	if !strings.Contains(string(encoded), "uriPreview") {
		t.Error("the bundle does not name the accounts it carries")
	}
}

func TestWrongPassphraseChangesNothing(t *testing.T) {
	ctx := context.Background()
	from := newInstance(t)
	seed(t, from)

	exported, err := Export(ctx, from, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}

	target := newInstance(t)
	if _, err := exported.Apply(ctx, target, ImportOptions{Passphrase: "not-the-passphrase"}); err != ErrWrongPassphrase {
		t.Fatalf("a wrong passphrase returned %v", err)
	}
	if _, err := exported.Apply(ctx, target, ImportOptions{}); err != ErrPassphraseRequired {
		t.Fatalf("a missing passphrase returned %v", err)
	}

	sources, err := target.Store.ListMongoSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("%d accounts were written despite the failure", len(sources))
	}
}

// Importing the same bundle twice must be a no-op the second time — that is
// what makes it safe to re-run against a live installation.
func TestImportingTwiceAddsNothingTheSecondTime(t *testing.T) {
	ctx := context.Background()
	from := newInstance(t)
	seed(t, from)

	exported, err := Export(ctx, from, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}

	target := newInstance(t)
	if _, err := exported.Apply(ctx, target, ImportOptions{Passphrase: passphrase}); err != nil {
		t.Fatal(err)
	}
	again, err := exported.Apply(ctx, target, ImportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	if again.Applied() != 0 {
		t.Fatalf("the second import changed %d entries: %s", again.Applied(), again.Summary())
	}
	if again.Configs.Skipped != 1 {
		t.Errorf("the existing config was not reported as skipped: %s", again.Summary())
	}
}

func TestOverwriteReplacesWhatIsAlreadyThere(t *testing.T) {
	ctx := context.Background()
	from := newInstance(t)
	seed(t, from)

	exported, err := Export(ctx, from, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}

	target := newInstance(t)
	if _, err := exported.Apply(ctx, target, ImportOptions{Passphrase: passphrase}); err != nil {
		t.Fatal(err)
	}

	// Change the config on the target, then import over it.
	job, err := target.Store.JobByName(ctx, "orders-archive")
	if err != nil {
		t.Fatal(err)
	}
	job.Config.Source.Collection = "invoices"
	if err := target.Store.SaveJob(ctx, &job); err != nil {
		t.Fatal(err)
	}

	result, err := exported.Apply(ctx, target, ImportOptions{Passphrase: passphrase, Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Configs.Updated != 1 {
		t.Fatalf("the config was not replaced: %s", result.Summary())
	}

	restored, err := target.Store.JobByName(ctx, "orders-archive")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Config.Source.Collection != "orders" {
		t.Errorf("the collection is %q after the overwrite", restored.Config.Source.Collection)
	}
	// One config, not two — an overwrite must not leave a duplicate behind.
	jobs, err := target.Store.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("%d configs after the overwrite", len(jobs))
	}
}

// A bundle exported without credentials is the one to keep in version control:
// it needs no passphrase, and it cannot create an account it has no key for.
func TestExportWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	from := newInstance(t)
	seed(t, from)

	exported, err := Export(ctx, from, ExportOptions{WithoutSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if exported.HasSecrets() {
		t.Fatal("a bundle exported without credentials reports that it has them")
	}
	if exported.MongoSources[0].URI != "" {
		t.Error("the connection string was exported anyway")
	}

	// Into an empty installation: the accounts cannot be created, so the config
	// that names them cannot be either, and every one of those is reported.
	empty := newInstance(t)
	result, err := exported.Apply(ctx, empty, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied() != 0 {
		t.Errorf("something was created without credentials: %s", result.Summary())
	}
	if len(result.Notes) == 0 {
		t.Error("nothing explained why the import was empty")
	}

	// Into an installation that already has the accounts: the configs land.
	target := newInstance(t)
	seedAccounts(t, target)
	result, err = exported.Apply(ctx, target, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Configs.Created != 1 {
		t.Fatalf("the config did not import: %s (%v)", result.Summary(), result.Notes)
	}
}

// A file that is not a bundle must say so rather than half-importing.
func TestDecodeRejectsAnythingElse(t *testing.T) {
	for _, body := range []string{`{"name":"orders-archive"}`, `[]`, `not json at all`} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("%q was accepted as a bundle", body)
		}
	}
}

// The scrypt parameters come out of a file somebody else wrote.
func TestAbsurdKDFParametersAreRefused(t *testing.T) {
	b := &Bundle{Feather: Header{Version: Version, Secrets: &Envelope{
		KDF: "scrypt", N: 1 << 30, R: 8, P: 1, Salt: "AAAAAAAAAAAAAAAAAAAAAA==",
	}}}
	if _, err := b.opener(passphrase); err == nil {
		t.Fatal("a bundle asking for a 1GB key derivation was accepted")
	}
}

func TestFileNameIsUsable(t *testing.T) {
	b := &Bundle{}
	b.Feather.Instance = "Production / Mumbai"
	name := b.FileName()
	if strings.ContainsAny(name, ` /\`) {
		t.Errorf("the download name is %q", name)
	}
	if !strings.HasPrefix(name, "feather-production-mumbai-") || !strings.HasSuffix(name, ".json") {
		t.Errorf("the download name is %q", name)
	}
}

// --- helpers ---------------------------------------------------------------

func newInstance(t *testing.T) *app.App {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	env := &config.Env{
		DataDir:           dir,
		DBPath:            filepath.Join(dir, "feather.db"),
		MasterKeyPath:     filepath.Join(dir, "master.key"),
		SpoolDir:          filepath.Join(dir, "spool"),
		HTTPAddr:          "127.0.0.1:0",
		MaxConcurrentJobs: 1,
	}
	instance, err := app.Open(ctx, env, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close(ctx) })
	return instance
}

func seedAccounts(t *testing.T, a *app.App) {
	t.Helper()
	ctx := context.Background()

	source := store.MongoSourceRecord{
		Alias: "prod", URI: "mongodb://user:hunter2@127.0.0.1:27017", Enabled: true,
	}
	if err := a.Store.SaveMongoSource(ctx, &source); err != nil {
		t.Fatal(err)
	}

	s3 := store.DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "cold", Provider: config.ProviderS3, Region: "ap-south-1", Bucket: "archive",
		AccessKeyID: "AKIA", SecretAccessKey: "very-secret-key", Prefix: "feather",
	}}
	if err := a.Store.SaveDestination(ctx, &s3); err != nil {
		t.Fatal(err)
	}

	// A Drive account exercises the credentials the S3 half never touches.
	drive := store.DestinationRecord{Enabled: true, Destination: config.Destination{
		Alias: "drive", Provider: config.ProviderGDrive, DriveFolderID: "1AbCfolder",
		ServiceAccountJSON: `{"type":"service_account","private_key":"private-key-material"}`,
	}}
	if err := a.Store.SaveDestination(ctx, &drive); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, a *app.App) {
	t.Helper()
	ctx := context.Background()

	seedAccounts(t, a)
	if err := a.Store.SetMeta(ctx, app.MetaInstanceName, "production"); err != nil {
		t.Fatal(err)
	}

	enabled := true
	record := store.JobRecord{Config: &config.JobConfig{
		Name:    "orders-archive",
		Enabled: &enabled,
		Source: config.Source{
			MongoSource: "prod", Database: "shop", Collection: "orders",
			Filter: json.RawMessage(`{"status":"completed"}`),
		},
		Output:       config.Output{KeyTemplate: "archive/{config}-{runId}-{seq}{ext}"},
		Destinations: []string{"cold", "drive"},
		Schedule:     config.Schedule{Every: "1d", At: "02:30", Timezone: "Asia/Kolkata"},
		Limits:       config.Limits{MaxRuntime: config.Duration(2 * time.Hour)},
	}}
	if err := a.Store.SaveJob(ctx, &record); err != nil {
		t.Fatal(err)
	}
}
