package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	cases := map[string]ByteSize{
		"1024":    1024,
		"256MB":   256 << 20,
		"256 MB":  256 << 20,
		"8.0 MiB": 8 << 20,
		"1gb":     1 << 30,
		"1.5GB":   1536 << 20,
		"512kb":   512 << 10,
		"2TB":     2 << 40,
	}
	for input, want := range cases {
		got, err := ParseByteSize(input)
		if err != nil {
			t.Errorf("%q: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("%q parsed to %d, want %d", input, got, want)
		}
	}

	for _, bad := range []string{"", "lots", "MB", "12 parsecs"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("%q should not have parsed", bad)
		}
	}
}

// The form renders a stored size with HumanBytes and parses whatever comes
// back, so the two have to agree or saving a config unedited would change it.
func TestByteSizeSurvivesTheFormRoundTrip(t *testing.T) {
	for _, size := range []ByteSize{8 << 20, 256 << 20, 1 << 30, 512 << 10} {
		rendered := size.String()
		parsed, err := ParseByteSize(rendered)
		if err != nil {
			t.Errorf("%d rendered as %q, which does not parse: %v", size, rendered, err)
			continue
		}
		if parsed != size {
			t.Errorf("%d rendered as %q and parsed back as %d", size, rendered, parsed)
		}
	}
}

func TestValidateKeyTemplate(t *testing.T) {
	valid := []string{
		"feather/{db}/{collection}/{yyyy}/{MM}/{dd}/{config}-{runId}-{seq}{ext}",
		"{config}-{ts}-{seq}{ext}",
		"archive/plain-name-{runId}.gz",
	}
	for _, template := range valid {
		if err := ValidateKeyTemplate(template); err != nil {
			t.Errorf("%q was rejected: %v", template, err)
		}
	}

	// A typo would otherwise be written into every object key literally.
	err := ValidateKeyTemplate("archive/{colletion}/{runId}{ext}")
	if err == nil {
		t.Fatal("a misspelled variable was accepted")
	}
	if !strings.Contains(err.Error(), "{colletion}") {
		t.Errorf("the error does not point at the typo: %v", err)
	}
}

func TestPrepareFillsDefaultsAndResolvesSchedule(t *testing.T) {
	job := &JobConfig{
		Name:         "orders-archive",
		Source:       Source{MongoSource: "prod", Database: "shop", Collection: "orders"},
		Destinations: []string{"cold"},
		Schedule:     Schedule{Every: "2d", At: "02:30", Timezone: "Asia/Kolkata"},
	}
	if err := job.Prepare(); err != nil {
		t.Fatal(err)
	}

	if job.Compression.Codec != CodecGzip || job.Compression.Level != 6 {
		t.Errorf("compression defaults are %s/%d", job.Compression.Codec, job.Compression.Level)
	}
	if job.Output.KeyTemplate != DefaultKeyTemplate {
		t.Errorf("key template default is %q", job.Output.KeyTemplate)
	}
	if job.Schedule.EveryDuration != 48*time.Hour {
		t.Errorf("interval resolved to %s", job.Schedule.EveryDuration)
	}
	if job.Schedule.AtHour != 2 || job.Schedule.AtMinute != 30 {
		t.Errorf("time of day resolved to %02d:%02d", job.Schedule.AtHour, job.Schedule.AtMinute)
	}
	if job.Schedule.Location == nil || job.Schedule.Location.String() != "Asia/Kolkata" {
		t.Error("the timezone was not resolved")
	}
	// Deletion must stay off unless it was asked for.
	if job.Retention.DeleteAfterArchive {
		t.Error("deletion defaulted to on")
	}
}

// Archiving everything and then emptying the collection is almost never what
// anyone means, so that combination has to be spelled out.
func TestEmptyFilterWithDeletionIsRejected(t *testing.T) {
	job := &JobConfig{
		Name:         "dangerous",
		Source:       Source{MongoSource: "prod", Database: "shop", Collection: "orders"},
		Destinations: []string{"cold"},
		Retention:    Retention{DeleteAfterArchive: true},
		Schedule:     Schedule{Every: "1d"},
	}
	if err := job.Prepare(); err == nil {
		t.Fatal("an empty filter with deletion on was accepted")
	}

	// A cap on the run makes the intent explicit, and is allowed.
	job.Limits.MaxDocsPerRun = 1000
	if err := job.Prepare(); err != nil {
		t.Fatalf("a capped run should be allowed: %v", err)
	}
}

func TestValidateChecksAccountsExist(t *testing.T) {
	known := Catalog{
		Sources:      map[string]MongoSource{"prod": {Alias: "prod"}},
		Destinations: map[string]Destination{"cold": {Alias: "cold"}},
	}

	job := &JobConfig{
		Name:         "orders-archive",
		Source:       Source{MongoSource: "prod", Database: "shop", Collection: "orders", Filter: json.RawMessage(`{"a":1}`)},
		Destinations: []string{"cold"},
		Schedule:     Schedule{Every: "1d"},
	}
	if err := job.Validate(known); err != nil {
		t.Fatalf("a valid config was rejected: %v", err)
	}

	job.Source.MongoSource = "missing"
	if err := job.Validate(known); err == nil {
		t.Error("an unknown MongoDB account was accepted")
	}

	job.Source.MongoSource = "prod"
	job.Destinations = []string{"nowhere"}
	if err := job.Validate(known); err == nil {
		t.Error("an unknown destination was accepted")
	}
}

// A config's fingerprint is recorded with every run, so it must change when the
// config does and stay put when it does not.
func TestComputeSHAIsStableAndSensitive(t *testing.T) {
	job := &JobConfig{
		Name:         "orders-archive",
		Source:       Source{MongoSource: "prod", Database: "shop", Collection: "orders"},
		Destinations: []string{"cold"},
		Schedule:     Schedule{Every: "1d"},
	}
	first := job.ComputeSHA()
	if first == "" || first != job.ComputeSHA() {
		t.Fatal("the fingerprint is not stable")
	}

	job.Retention.DeleteAfterArchive = true
	if job.ComputeSHA() == first {
		t.Fatal("turning deletion on did not change the fingerprint")
	}
}

func TestRedactMongoURI(t *testing.T) {
	cases := map[string]string{
		"mongodb://user:hunter2@127.0.0.1:27017":                 "mongodb://user:•••@127.0.0.1:27017",
		"mongodb+srv://admin:s3cret@cluster.example.mongodb.net": "mongodb+srv://admin:•••@cluster.example.mongodb.net",
		"mongodb://127.0.0.1:27017":                              "mongodb://127.0.0.1:27017",
	}
	for input, want := range cases {
		got := RedactMongoURI(input)
		if got != want {
			t.Errorf("%q redacted to %q, want %q", input, got, want)
		}
		if strings.Contains(got, "hunter2") || strings.Contains(got, "s3cret") {
			t.Errorf("%q still contains its password", got)
		}
	}
}

func TestPreviewKeyRendersEveryVariable(t *testing.T) {
	job := &JobConfig{
		Name:   "orders-archive",
		Source: Source{Database: "shop", Collection: "orders"},
		Output: Output{KeyTemplate: "{config}/{db}/{collection}/{yyyy}/{MM}/{dd}/{ts}-{runId}-{seq}{ext}"},
	}
	preview := job.PreviewKey(time.Date(2026, 5, 14, 2, 30, 0, 0, time.UTC))

	for _, want := range []string{"orders-archive", "shop", "orders", "2026", "05", "14", "00001", ".ndjson.gz"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview %q is missing %q", preview, want)
		}
	}
	if strings.Contains(preview, "{") {
		t.Errorf("preview still holds an unexpanded variable: %q", preview)
	}
}

const testServiceAccountKey = `{"type":"service_account","client_email":"feather@example.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----"}`

func TestValidateDestinationAcceptsWhatEachProviderNeeds(t *testing.T) {
	cases := map[string]struct {
		destination Destination
		wantErr     string
	}{
		"s3 with a bucket and region": {
			destination: Destination{Alias: "cold", Provider: ProviderS3, Region: "ap-south-1", Bucket: "archive"},
		},
		"oracle without an endpoint": {
			destination: Destination{Alias: "oci", Provider: ProviderOCI, Region: "ap-mumbai-1", Bucket: "archive"},
			wantErr:     "S3-compatible endpoint",
		},
		"drive with a service account and a folder": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive,
				ServiceAccountJSON: testServiceAccountKey, DriveFolderID: "1AbC"},
		},
		"drive with a service account and no folder": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive, ServiceAccountJSON: testServiceAccountKey},
			wantErr:     "folder ID is required",
		},
		"drive with a service account that is not JSON": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive,
				ServiceAccountJSON: "not a key file", DriveFolderID: "1AbC"},
			wantErr: "not valid JSON",
		},
		"drive with an oauth client, not yet connected": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive,
				OAuthClientID: "id.apps.googleusercontent.com", OAuthClientSecret: "shh"},
		},
		"drive with half an oauth client": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive, OAuthClientID: "id.apps.googleusercontent.com"},
			wantErr:     "client ID and a client secret",
		},
		"drive with both ways in": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive, ServiceAccountJSON: testServiceAccountKey,
				DriveFolderID: "1AbC", OAuthClientID: "id", OAuthClientSecret: "shh"},
			wantErr: "keep one",
		},
		"drive with no credentials at all": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive, DriveFolderID: "1AbC"},
			wantErr:     "service account key or an OAuth client",
		},
		"drive impersonating without a service account": {
			destination: Destination{Alias: "drive", Provider: ProviderGDrive,
				OAuthClientID: "id", OAuthClientSecret: "shh", DriveSubject: "ops@example.com"},
			wantErr: "only possible with a service account",
		},
		"an unknown provider": {
			destination: Destination{Alias: "x", Provider: Provider("dropbox")},
			wantErr:     "unknown provider",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateDestination(tc.destination)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("rejected a valid destination: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("accepted a destination that should have failed on %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// A Drive destination is stored before it is authorised, so "ready" is what
// keeps an unconnected one out of the registry and out of a config's choices.
func TestDriveDestinationIsNotReadyUntilItIsConnected(t *testing.T) {
	pending := Destination{Alias: "drive", Provider: ProviderGDrive,
		OAuthClientID: "id.apps.googleusercontent.com", OAuthClientSecret: "shh"}
	if pending.Ready() {
		t.Error("an unconnected OAuth destination reports itself ready")
	}

	connected := pending
	connected.OAuthRefreshToken = "1//refresh"
	if !connected.Ready() {
		t.Error("a connected OAuth destination is not ready")
	}

	withKey := Destination{Alias: "drive", Provider: ProviderGDrive,
		ServiceAccountJSON: testServiceAccountKey, DriveFolderID: "1AbC"}
	if !withKey.Ready() {
		t.Error("a service account destination is not ready")
	}

	if !(Destination{Provider: ProviderS3}).Ready() {
		t.Error("an S3 destination is never pending authorisation")
	}
}

func TestDriveRootDefaultsToMyDrive(t *testing.T) {
	if got := (Destination{}).DriveRoot(); got != "root" {
		t.Errorf("an unnamed folder resolved to %q, want root", got)
	}
	if got := (Destination{DriveFolderID: "1AbC"}).DriveRoot(); got != "1AbC" {
		t.Errorf("a named folder resolved to %q", got)
	}
}
