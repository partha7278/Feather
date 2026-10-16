package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envKeys is every variable LoadEnv reads. Each test starts from a known state
// for all of them, so a developer's shell cannot change the outcome.
var envKeys = []string{
	"FEATHER_ENV", "LOG_LEVEL", "LOG_JSON", "FEATHER_DATA_DIR", "FEATHER_DB",
	"FEATHER_MASTER_KEY_FILE", "FEATHER_MASTER_KEY", "FEATHER_SPOOL_DIR",
	"FEATHER_HTTP_ADDR", "FEATHER_BASE_URL", "FEATHER_TRUSTED_PROXIES",
	"FEATHER_MAX_CONCURRENT_JOBS",
}

// unsetEnv removes a variable for the duration of the test and restores it
// afterwards. t.Setenv registers the restore; the Unsetenv makes the key truly
// absent, which matters because godotenv only fills in keys that are absent.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func clearFeatherEnv(t *testing.T) {
	t.Helper()
	for _, key := range envKeys {
		unsetEnv(t, key)
	}
}

func TestLoadEnvFallsBackToWorkingDefaults(t *testing.T) {
	clearFeatherEnv(t)
	dataDir := t.TempDir()
	t.Setenv("FEATHER_DATA_DIR", dataDir)

	// A missing env file is normal on a VPS that injects variables via systemd.
	env, err := LoadEnv(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("a missing env file was treated as an error: %v", err)
	}

	checks := []struct{ field, got, want string }{
		{"Environment", env.Environment, "production"},
		{"LogLevel", env.LogLevel, "info"},
		{"DataDir", env.DataDir, dataDir},
		{"DBPath", env.DBPath, filepath.Join(dataDir, "feather.db")},
		{"MasterKeyPath", env.MasterKeyPath, filepath.Join(dataDir, "master.key")},
		{"MasterKey", env.MasterKey, ""},
		{"SpoolDir", env.SpoolDir, filepath.Join(dataDir, "spool")},
		{"HTTPAddr", env.HTTPAddr, "127.0.0.1:8477"},
		{"BaseURL", env.BaseURL, ""},
		{"TrustedProxies", env.TrustedProxies, ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s defaulted to %q, want %q", c.field, c.got, c.want)
		}
	}
	if !env.LogJSON {
		t.Error("JSON logging should be on by default")
	}
	if env.MaxConcurrentJobs != 2 {
		t.Errorf("MaxConcurrentJobs defaulted to %d, want 2", env.MaxConcurrentJobs)
	}
}

func TestLoadEnvHonoursOverridesAndDerivesPathsFromTheDataDir(t *testing.T) {
	clearFeatherEnv(t)
	t.Setenv("FEATHER_DATA_DIR", "/srv/feather")
	t.Setenv("FEATHER_ENV", "staging")
	t.Setenv("LOG_LEVEL", " debug ")
	t.Setenv("LOG_JSON", "false")
	t.Setenv("FEATHER_HTTP_ADDR", "0.0.0.0:9000")
	t.Setenv("FEATHER_BASE_URL", "https://feather.example.com/")
	t.Setenv("FEATHER_SPOOL_DIR", "/tmp/spool")

	env, err := LoadEnv(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatal(err)
	}
	if env.Environment != "staging" || env.LogLevel != "debug" || env.LogJSON {
		t.Errorf("overrides not applied: env=%q level=%q json=%v", env.Environment, env.LogLevel, env.LogJSON)
	}
	if env.DBPath != filepath.Join("/srv/feather", "feather.db") {
		t.Errorf("database path did not follow the data dir: %q", env.DBPath)
	}
	if env.SpoolDir != "/tmp/spool" {
		t.Errorf("an explicit spool dir was ignored: %q", env.SpoolDir)
	}
	// A trailing slash would produce "//login" when links are joined onto it.
	if env.BaseURL != "https://feather.example.com" {
		t.Errorf("the trailing slash was not trimmed from the base URL: %q", env.BaseURL)
	}
	if env.HTTPAddr != "0.0.0.0:9000" {
		t.Errorf("listen address is %q", env.HTTPAddr)
	}
}

// Zero workers would mean the scheduler never runs anything, silently.
func TestLoadEnvNeverAllowsFewerThanOneConcurrentJob(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"0", 1},
		{"-4", 1},
		{"1", 1},
		{" 5 ", 5},
		{"many", 2}, // unparseable falls back to the default
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			clearFeatherEnv(t)
			t.Setenv("FEATHER_DATA_DIR", t.TempDir())
			t.Setenv("FEATHER_MAX_CONCURRENT_JOBS", tc.raw)

			env, err := LoadEnv(filepath.Join(t.TempDir(), "absent.env"))
			if err != nil {
				t.Fatal(err)
			}
			if env.MaxConcurrentJobs != tc.want {
				t.Errorf("FEATHER_MAX_CONCURRENT_JOBS=%q gave %d, want %d", tc.raw, env.MaxConcurrentJobs, tc.want)
			}
		})
	}
}

// Real environment variables win over the file, so systemd can override a
// value without editing .env.
func TestLoadEnvReadsTheFileWithoutOverridingTheEnvironment(t *testing.T) {
	clearFeatherEnv(t)
	t.Setenv("FEATHER_DATA_DIR", t.TempDir())
	t.Setenv("LOG_LEVEL", "warn")

	path := filepath.Join(t.TempDir(), "feather.env")
	body := "FEATHER_HTTP_ADDR=127.0.0.1:9999\nLOG_LEVEL=debug\nFEATHER_MAX_CONCURRENT_JOBS=4\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if env.HTTPAddr != "127.0.0.1:9999" {
		t.Errorf("the env file's listen address was not read: %q", env.HTTPAddr)
	}
	if env.MaxConcurrentJobs != 4 {
		t.Errorf("the env file's concurrency was not read: %d", env.MaxConcurrentJobs)
	}
	if env.LogLevel != "warn" {
		t.Errorf("the env file overrode a real environment variable: %q", env.LogLevel)
	}
}

func TestLoadEnvLooksForDotEnvInTheWorkingDirectoryByDefault(t *testing.T) {
	clearFeatherEnv(t)
	t.Setenv("FEATHER_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("FEATHER_ENV=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	env, err := LoadEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if env.Environment != "from-dotenv" {
		t.Errorf("the default .env was not read: %q", env.Environment)
	}
}

func TestLoadEnvReportsAnEnvFileItCannotParse(t *testing.T) {
	clearFeatherEnv(t)
	t.Setenv("FEATHER_DATA_DIR", t.TempDir())

	// A directory exists but cannot be read as an env file; that is a real
	// misconfiguration, not the "no .env" case.
	dir := t.TempDir()
	if _, err := LoadEnv(dir); err == nil {
		t.Fatal("expected an unreadable env file to be reported")
	} else if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestEnvGettersTrimAndFallBack(t *testing.T) {
	const key = "FEATHER_TEST_GETTER"

	stringCases := []struct{ raw, want string }{
		{"", "fallback"},
		{"   ", "fallback"},
		{" value ", "value"},
	}
	for _, tc := range stringCases {
		t.Setenv(key, tc.raw)
		if got := getString(key, "fallback"); got != tc.want {
			t.Errorf("getString(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	intCases := []struct {
		raw  string
		want int
	}{
		{"", 7}, {"12", 12}, {" -3 ", -3}, {"1.5", 7}, {"ten", 7},
	}
	for _, tc := range intCases {
		t.Setenv(key, tc.raw)
		if got := getInt(key, 7); got != tc.want {
			t.Errorf("getInt(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	boolCases := []struct {
		raw      string
		fallback bool
		want     bool
	}{
		{"", true, true},
		{"", false, false},
		{"false", true, false},
		{" 1 ", false, true},
		{"TRUE", false, true},
		{"yes", false, false}, // not a Go bool: falls back rather than guessing
	}
	for _, tc := range boolCases {
		t.Setenv(key, tc.raw)
		if got := getBool(key, tc.fallback); got != tc.want {
			t.Errorf("getBool(%q, %v) = %v, want %v", tc.raw, tc.fallback, got, tc.want)
		}
	}

	unsetEnv(t, key)
	if getString(key, "fb") != "fb" || getInt(key, 3) != 3 || !getBool(key, true) {
		t.Error("an unset variable did not fall back")
	}
}

func TestProviderKnowsWhichSpeakS3AndHowToNameItself(t *testing.T) {
	cases := []struct {
		provider Provider
		isS3     bool
		label    string
	}{
		{ProviderS3, true, "AWS S3"},
		{ProviderOCI, true, "Oracle Cloud"},
		{ProviderGDrive, false, "Google Drive"},
		{Provider("dropbox"), false, "AWS S3"},
	}
	for _, tc := range cases {
		if got := tc.provider.IsS3(); got != tc.isS3 {
			t.Errorf("%q.IsS3() = %v, want %v", tc.provider, got, tc.isS3)
		}
		if got := tc.provider.Label(); got != tc.label {
			t.Errorf("%q.Label() = %q, want %q", tc.provider, got, tc.label)
		}
	}
}

func TestValidateDestinationRejectsIncompleteS3Accounts(t *testing.T) {
	cases := map[string]struct {
		destination Destination
		wantErr     string
	}{
		"no alias": {
			destination: Destination{Provider: ProviderS3, Region: "us-east-1", Bucket: "b"},
			wantErr:     "alias is required",
		},
		"no bucket": {
			destination: Destination{Alias: "cold", Provider: ProviderS3, Region: "us-east-1"},
			wantErr:     "bucket is required",
		},
		"no region": {
			destination: Destination{Alias: "cold", Provider: ProviderS3, Bucket: "b"},
			wantErr:     "region is required",
		},
		"custom endpoint without keys": {
			destination: Destination{Alias: "minio", Provider: ProviderS3, Region: "us-east-1", Bucket: "b",
				Endpoint: "https://minio.local"},
			wantErr: "access key and secret are required",
		},
		"access key without its secret": {
			destination: Destination{Alias: "cold", Provider: ProviderS3, Region: "us-east-1", Bucket: "b",
				AccessKeyID: "AKIA"},
			wantErr: "without its secret",
		},
		"oracle with endpoint and keys": {
			destination: Destination{Alias: "oci", Provider: ProviderOCI, Region: "ap-mumbai-1", Bucket: "b",
				Endpoint:    "https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com",
				AccessKeyID: "id", SecretAccessKey: "secret"},
		},
		"aws relying on an instance role": {
			destination: Destination{Alias: "cold", Provider: ProviderS3, Region: "us-east-1", Bucket: "b"},
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

func TestRedactSecretsFromErrorCleansTheMessage(t *testing.T) {
	err := errors.New("dial failed: mongodb://admin:hunter2@db.example.com:27017")
	got := RedactSecretsFromError(err)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the password survived: %s", got)
	}
	if !strings.Contains(got, "db.example.com:27017") || !strings.HasPrefix(got, "dial failed:") {
		t.Errorf("redaction lost the useful part of the message: %s", got)
	}
}

func TestLoadLegacyEnvImportsSourcesAndDestinations(t *testing.T) {
	unsetEnv(t, "MONGO_URI")
	path := filepath.Join(t.TempDir(), "old.env")
	body := strings.Join([]string{
		"MONGO_URI=mongodb://user:pw@db:27017",
		"FEATHER_DESTINATIONS= cold , oci-backup ,,",
		"DEST_COLD_REGION=ap-south-1",
		"DEST_COLD_BUCKET=archive",
		"DEST_COLD_PREFIX=/feather/",
		"DEST_OCI_BACKUP_PROVIDER=OCI",
		"DEST_OCI_BACKUP_REGION=ap-mumbai-1",
		"DEST_OCI_BACKUP_BUCKET=oci-archive",
		"DEST_OCI_BACKUP_ENDPOINT=https://ns.compat.objectstorage.ap-mumbai-1.oraclecloud.com",
		"DEST_OCI_BACKUP_ACCESS_KEY_ID=id",
		"DEST_OCI_BACKUP_SECRET_ACCESS_KEY=secret",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	legacy, err := LoadLegacyEnv(path)
	if err != nil {
		t.Fatal(err)
	}

	source, ok := legacy.Sources["default"]
	if !ok || len(legacy.Sources) != 1 {
		t.Fatalf("expected one source under the default alias, got %v", legacy.Sources)
	}
	if source.URI != "mongodb://user:pw@db:27017" || source.AuthSource != "admin" || source.AppName != "feather" {
		t.Errorf("source not imported with its defaults: %+v", source)
	}

	if len(legacy.Destinations) != 2 {
		t.Fatalf("expected two destinations, got %d: %v", len(legacy.Destinations), legacy.Destinations)
	}
	cold := legacy.Destinations["cold"]
	if cold.Provider != ProviderS3 || cold.Bucket != "archive" || cold.Region != "ap-south-1" {
		t.Errorf("cold imported as %+v", cold)
	}
	if cold.Prefix != "feather/" {
		t.Errorf("the leading slash was not trimmed from the prefix: %q", cold.Prefix)
	}
	if cold.ForcePathStyle {
		t.Error("an AWS destination was forced to path style without being asked")
	}
	oci := legacy.Destinations["oci-backup"]
	if oci.Provider != ProviderOCI {
		t.Errorf("provider was not lower-cased: %q", oci.Provider)
	}
	if !oci.ForcePathStyle {
		t.Error("Oracle only speaks path style, but the import left it off")
	}

	// Importing must not change how the running daemon behaves.
	if _, set := os.LookupEnv("MONGO_URI"); set {
		t.Error("reading a legacy file leaked its values into the process environment")
	}
}

func TestLoadLegacyEnvHonoursExplicitMongoSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.env")
	body := "MONGO_URI=mongodb://db\nMONGO_ALIAS=prod\nMONGO_AUTH_SOURCE=users\nMONGO_APP_NAME=nightly\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := LoadLegacyEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	want := MongoSource{Alias: "prod", URI: "mongodb://db", AuthSource: "users", AppName: "nightly"}
	if got := legacy.Sources["prod"]; got != want {
		t.Errorf("source is %+v, want %+v", got, want)
	}
	if len(legacy.Destinations) != 0 {
		t.Errorf("no destinations were listed, but %d were imported", len(legacy.Destinations))
	}
}

func TestLoadLegacyEnvRefusesWhatItCannotImport(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadLegacyEnv(filepath.Join(dir, "missing.env")); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing file gave %v", err)
	}

	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("FEATHER_DESTINATIONS=cold\nDEST_COLD_REGION=us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadLegacyEnv(bad)
	if err == nil {
		t.Fatal("a destination without a bucket was imported")
	}
	if !strings.Contains(err.Error(), `"cold"`) || !strings.Contains(err.Error(), "bucket") {
		t.Errorf("the error does not say which destination or what is wrong: %v", err)
	}
}

func TestEnvKeySegmentMatchesTheOldVariableNames(t *testing.T) {
	cases := map[string]string{
		"cold":           "COLD",
		"s3-backup-acct": "S3_BACKUP_ACCT",
		"oci.mumbai":     "OCI_MUMBAI",
		"Mixed_Case 2":   "MIXED_CASE_2",
		"":               "",
	}
	for alias, want := range cases {
		if got := envKeySegment(alias); got != want {
			t.Errorf("envKeySegment(%q) = %q, want %q", alias, got, want)
		}
	}
}

func TestSplitListDropsBlanksAndWhitespace(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", []string{}},
		{" , ,", []string{}},
		{"a", []string{"a"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := splitList(tc.raw)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
			t.Errorf("splitList(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
