package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var loadCatalog = Catalog{
	Sources:      map[string]MongoSource{"prod": {Alias: "prod"}, "legacy": {Alias: "legacy"}},
	Destinations: map[string]Destination{"cold": {Alias: "cold"}},
}

func writeJobFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func jobJSON(name, source string, extra string) string {
	fields := []string{}
	if name != "" {
		fields = append(fields, `"name":`+quote(name))
	}
	src := `"source":{"database":"shop","collection":"orders","filter":{"status":"done"}`
	if source != "" {
		src += `,"mongoSource":` + quote(source)
	}
	src += `}`
	fields = append(fields, src,
		`"destinations":["cold"]`,
		`"schedule":{"every":"1d"}`,
		`"output":{"keyTemplate":"archive/{collection}/{runId}-{seq}{ext}"}`)
	if extra != "" {
		fields = append(fields, extra)
	}
	return "{" + strings.Join(fields, ",") + "}"
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestLoadJobsReadsEveryJSONFileSortedByName(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "zeta.json", jobJSON("zeta", "prod", ""))
	writeJobFile(t, dir, "alpha.json", jobJSON("alpha", "prod", ""))
	// The name comes from the file when the config does not give one.
	writeJobFile(t, dir, "middle.json", jobJSON("", "prod", ""))
	writeJobFile(t, dir, "README.md", "not a config")
	if err := os.Mkdir(filepath.Join(dir, "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	jobs, err := LoadJobs(dir, loadCatalog)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, job := range jobs {
		names = append(names, job.Name)
		if job.SourcePath != filepath.Join(dir, job.Name+".json") {
			t.Errorf("%s records its path as %q", job.Name, job.SourcePath)
		}
		if job.SHA256 == "" {
			t.Errorf("%s was loaded without a fingerprint", job.Name)
		}
	}
	if got := strings.Join(names, ","); got != "alpha,middle,zeta" {
		t.Errorf("loaded %s, want alpha,middle,zeta in that order", got)
	}
}

// One bad file fails the whole load, and every problem is reported at once so
// the operator does not fix them one restart at a time.
func TestLoadJobsRefusesAPartiallyValidDirectory(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "good.json", jobJSON("good", "prod", ""))
	writeJobFile(t, dir, "broken.json", `{"name":`)
	writeJobFile(t, dir, "unknown.json", jobJSON("unknown", "nowhere", ""))

	jobs, err := LoadJobs(dir, loadCatalog)
	if err == nil {
		t.Fatalf("a directory with bad configs loaded %d jobs", len(jobs))
	}
	if jobs != nil {
		t.Error("a failed load still returned configs")
	}
	for _, want := range []string{"broken.json", "unknown.json", `"nowhere"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "good.json") {
		t.Errorf("the valid file was reported as a problem: %v", err)
	}
}

func TestLoadJobsRejectsTwoFilesWithTheSameName(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "a.json", jobJSON("orders", "prod", ""))
	writeJobFile(t, dir, "b.json", jobJSON("orders", "prod", ""))

	_, err := LoadJobs(dir, loadCatalog)
	if err == nil || !strings.Contains(err.Error(), `name "orders" is already used`) {
		t.Fatalf("duplicate names gave %v", err)
	}
}

func TestLoadJobsReportsAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := LoadJobs(missing, loadCatalog); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("a missing directory gave %v", err)
	}
}

// The strict reader refuses fields it does not know — a typo in
// "deleteAfterArchive" must not silently mean "false". The importer is lenient,
// because an old file naming a removed setting should still come across.
func TestLenientLoadToleratesOldFieldsAndFillsTheSource(t *testing.T) {
	dir := t.TempDir()
	writeJobFile(t, dir, "old.json", jobJSON("old", "", `"retiredSetting":true`))

	if _, err := LoadJobs(dir, loadCatalog); err == nil || !strings.Contains(err.Error(), "retiredSetting") {
		t.Fatalf("the strict reader accepted an unknown field: %v", err)
	}

	jobs, err := LoadJobsLenient(dir, loadCatalog, "legacy")
	if err != nil {
		t.Fatalf("the lenient reader refused an old file: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Source.MongoSource != "legacy" {
		t.Fatalf("the default MongoDB account was not filled in: %+v", jobs)
	}

	// An explicit source is kept, not replaced by the default.
	writeJobFile(t, dir, "old.json", jobJSON("old", "prod", ""))
	jobs, err = LoadJobsLenient(dir, loadCatalog, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Source.MongoSource != "prod" {
		t.Errorf("the explicit source was overwritten with %q", jobs[0].Source.MongoSource)
	}
}

func TestLoadJobReadsOneFile(t *testing.T) {
	dir := t.TempDir()
	path := writeJobFile(t, dir, "orders.json", jobJSON("orders", "prod", `"limits":{"maxRuntime":"2d"}`))

	job, err := LoadJob(path, loadCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "orders" || job.SourcePath != path {
		t.Errorf("loaded %q from %q", job.Name, job.SourcePath)
	}
	if job.Limits.MaxRuntime.Std() != 48*time.Hour {
		t.Errorf("maxRuntime resolved to %s", job.Limits.MaxRuntime.Std())
	}

	if _, err := LoadJob(filepath.Join(dir, "missing.json"), loadCatalog); err == nil {
		t.Error("a missing file loaded")
	}
	// Without a MongoDB account the job cannot run anywhere.
	noSource := writeJobFile(t, dir, "nosource.json", jobJSON("nosource", "", ""))
	if _, err := LoadJob(noSource, loadCatalog); err == nil || !strings.Contains(err.Error(), "mongoSource is required") {
		t.Errorf("a config with no MongoDB account gave %v", err)
	}
}

func TestFindJobNamesTheAlternativesWhenItMisses(t *testing.T) {
	jobs := []*JobConfig{{Name: "orders"}, {Name: "events"}}

	found, err := FindJob(jobs, "events")
	if err != nil || found != jobs[1] {
		t.Fatalf("FindJob returned %v, %v", found, err)
	}

	_, err = FindJob(jobs, "invoices")
	if err == nil {
		t.Fatal("an unknown name was found")
	}
	if !strings.Contains(err.Error(), `"invoices"`) || !strings.Contains(err.Error(), "orders, events") {
		t.Errorf("the error does not help the operator pick: %v", err)
	}

	if _, err := FindJob(nil, "orders"); err == nil {
		t.Error("found a job in an empty list")
	}
}

func validPrepareJob() *JobConfig {
	return &JobConfig{
		Name:         "orders-archive",
		Source:       Source{Database: "shop", Collection: "orders", Filter: json.RawMessage(`{"a":1}`)},
		Destinations: []string{"cold"},
		Schedule:     Schedule{Every: "1d"},
	}
}

func TestPrepareRejectsConfigsItCannotMakeSenseOf(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*JobConfig)
		wantErr string
	}{
		{"a name with a slash", func(c *JobConfig) { c.Name = "a/b" }, "name"},
		{"an empty name", func(c *JobConfig) { c.Name = "" }, "name"},
		{"no database", func(c *JobConfig) { c.Source.Database = "" }, "source.database is required"},
		{"no collection", func(c *JobConfig) { c.Source.Collection = "" }, "source.collection is required"},
		{"a filter that is not JSON", func(c *JobConfig) { c.Source.Filter = json.RawMessage(`{a:`) }, "source.filter is not valid JSON"},
		{"a projection that is not JSON", func(c *JobConfig) { c.Source.Projection = json.RawMessage(`{x`) }, "source.projection is not valid JSON"},
		{"an unknown read preference", func(c *JobConfig) { c.Source.ReadPreference = "fastest" }, "readPreference"},
		{"an unsupported format", func(c *JobConfig) { c.Output.Format = "csv" }, "output.format"},
		{"an unknown codec", func(c *JobConfig) { c.Compression.Codec = "lz4" }, "compression.codec"},
		{"gzip above level 9", func(c *JobConfig) { c.Compression.Level = 10 }, "out of range for gzip"},
		{"gzip below level 1", func(c *JobConfig) { c.Compression.Level = -1 }, "out of range for gzip"},
		{"zstd above level 4", func(c *JobConfig) { c.Compression = Compression{Codec: CodecZstd, Level: 5} }, "out of range for zstd"},
		{"no destinations", func(c *JobConfig) { c.Destinations = nil }, "at least one destination"},
		{"a destination listed twice", func(c *JobConfig) { c.Destinations = []string{"cold", "cold"} }, "listed twice"},
		{"a key template with an unknown variable", func(c *JobConfig) { c.Output.KeyTemplate = "{nope}/{runId}" }, "output.keyTemplate"},
		{"no schedule", func(c *JobConfig) { c.Schedule = Schedule{} }, `either "cron" or "every"`},
		{"both cron and every", func(c *JobConfig) { c.Schedule.Cron = "0 2 * * *" }, "pick one"},
		{"an unknown timezone", func(c *JobConfig) { c.Schedule.Timezone = "Mars/Olympus" }, "schedule.timezone"},
		{"an unparseable interval", func(c *JobConfig) { c.Schedule.Every = "fortnightly" }, "schedule.every"},
		{"an interval under a minute", func(c *JobConfig) { c.Schedule.Every = "30s" }, "one minute minimum"},
		{"a malformed time of day", func(c *JobConfig) { c.Schedule.At = "25:00" }, "HH:MM"},
		{"a time of day on a sub-daily interval", func(c *JobConfig) { c.Schedule = Schedule{Every: "6h", At: "02:00"} }, "at least 24h"},
		{"a filter with a bad time token", func(c *JobConfig) { c.Source.Filter = json.RawMessage(`{"at":"{{nwo}}"}`) }, "source.filter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := validPrepareJob()
			tc.mutate(job)
			err := job.Prepare()
			if err == nil {
				t.Fatalf("accepted a config that should fail on %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}

	// The baseline the cases mutate has to be valid, or they prove nothing.
	if err := validPrepareJob().Prepare(); err != nil {
		t.Fatalf("the baseline config is itself invalid: %v", err)
	}
}

func TestPrepareAcceptsEveryReadPreferenceAndZstd(t *testing.T) {
	for _, pref := range []string{"primary", "primaryPreferred", "secondary", "SecondaryPreferred", "nearest"} {
		job := validPrepareJob()
		job.Source.ReadPreference = pref
		if err := job.Prepare(); err != nil {
			t.Errorf("read preference %q was rejected: %v", pref, err)
		}
	}

	job := validPrepareJob()
	job.Compression = Compression{Codec: CodecZstd, Level: 3}
	if err := job.Prepare(); err != nil {
		t.Fatalf("zstd level 3 was rejected: %v", err)
	}
	if job.Extension() != ".ndjson.zst" {
		t.Errorf("zstd archives would be named %q", job.Extension())
	}

	cron := validPrepareJob()
	cron.Schedule = Schedule{Cron: "0 2 * * *"}
	if err := cron.Prepare(); err != nil {
		t.Fatalf("a cron schedule was rejected: %v", err)
	}
	if cron.Schedule.AtHour != -1 || cron.Schedule.EveryDuration != 0 {
		t.Errorf("a cron schedule resolved interval fields: %+v", cron.Schedule)
	}
}

// A written-out "projection": null would otherwise be stored and sent to Mongo
// as the literal projection null.
func TestPrepareTreatsANullProjectionAsAbsent(t *testing.T) {
	job := validPrepareJob()
	job.Source.Projection = json.RawMessage(` null `)
	if err := job.Prepare(); err != nil {
		t.Fatal(err)
	}
	if job.Source.Projection != nil {
		t.Errorf("projection is %q, want none", job.Source.Projection)
	}
}

// Absent means enabled and absent means verify: both safe directions. Only an
// explicit false turns either off.
func TestEnabledAndVerifyDefaultToOnUnlessExplicitlyOff(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		flag *bool
		want bool
	}{
		{"absent", nil, true},
		{"explicit true", &yes, true},
		{"explicit false", &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &JobConfig{Enabled: tc.flag, Retention: Retention{VerifyBeforeDelete: tc.flag}}
			if got := job.IsEnabled(); got != tc.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tc.want)
			}
			if got := job.ShouldVerify(); got != tc.want {
				t.Errorf("ShouldVerify() = %v, want %v", got, tc.want)
			}
		})
	}

	// And the same through JSON, which is how a config file says it.
	var job JobConfig
	if err := json.Unmarshal([]byte(`{"name":"x","retention":{}}`), &job); err != nil {
		t.Fatal(err)
	}
	if !job.IsEnabled() || !job.ShouldVerify() {
		t.Error("a config file that says nothing turned enabling or verification off")
	}
}

func TestDurationReadsDaysAndRoundTrips(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{`"2d"`, 48 * time.Hour, false},
		{`"1.5d"`, 36 * time.Hour, false},
		{`"90m"`, 90 * time.Minute, false},
		{`"6h30m"`, 6*time.Hour + 30*time.Minute, false},
		{`3600`, 0, true}, // a bare number has no unit, so it is refused
		{`"forever"`, 0, true},
		{`null`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tc.in), &d)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s was accepted as %s", tc.in, d.Std())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Std() != tc.want {
				t.Fatalf("%s parsed to %s, want %s", tc.in, d.Std(), tc.want)
			}

			// What feather writes, feather must be able to read back.
			encoded, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var back Duration
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("could not read back its own output %s: %v", encoded, err)
			}
			if back != d {
				t.Errorf("round trip changed %s into %s", d.Std(), back.Std())
			}
		})
	}
}
