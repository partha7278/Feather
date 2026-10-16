package dest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/retry"
)

func TestNewRegistryWithIsNotAliasedToTheCallersMap(t *testing.T) {
	given := map[string]Destination{"primary": newCheckedDestination("primary", nil)}
	registry := NewRegistryWith(given)

	given["intruder"] = newCheckedDestination("intruder", nil)
	delete(given, "primary")

	if got := registry.Aliases(); !reflect.DeepEqual(got, []string{"primary"}) {
		t.Errorf("registry changed with the caller's map: %v", got)
	}
}

func TestRegistryGetResolvesAConfiguredAlias(t *testing.T) {
	primary := newCheckedDestination("primary", nil)
	registry := NewRegistryWith(map[string]Destination{"primary": primary})

	got, err := registry.Get("primary")
	if err != nil {
		t.Fatal(err)
	}
	if got != primary {
		t.Errorf("Get returned %v, want the registered destination", got)
	}
}

func TestRegistryGetRejectsAnUnknownAlias(t *testing.T) {
	registry := NewRegistryWith(map[string]Destination{"primary": newCheckedDestination("primary", nil)})

	_, err := registry.Get("offsite")
	if err == nil {
		t.Fatal("an unconfigured alias resolved")
	}
	if !strings.Contains(err.Error(), `"offsite"`) {
		t.Errorf("error does not name the missing alias: %v", err)
	}
}

// Logs and catalogue rows list destinations in the order the config gives them.
func TestRegistryResolvePreservesTheConfiguredOrder(t *testing.T) {
	registry := NewRegistryWith(map[string]Destination{
		"a": newCheckedDestination("a", nil),
		"b": newCheckedDestination("b", nil),
		"c": newCheckedDestination("c", nil),
	})

	for _, order := range [][]string{{"c", "a", "b"}, {"b"}, {}, {"a", "c"}} {
		got, err := registry.Resolve(order)
		if err != nil {
			t.Fatalf("resolving %v: %v", order, err)
		}
		aliases := make([]string, 0, len(got))
		for _, d := range got {
			aliases = append(aliases, d.Alias())
		}
		if !reflect.DeepEqual(aliases, order) {
			t.Errorf("resolved %v as %v", order, aliases)
		}
	}
}

// One unknown alias fails the whole list: a job must not quietly write to fewer
// destinations than it was configured with.
func TestRegistryResolveFailsIfAnyAliasIsMissing(t *testing.T) {
	registry := NewRegistryWith(map[string]Destination{"a": newCheckedDestination("a", nil)})

	got, err := registry.Resolve([]string{"a", "gone"})
	if err == nil {
		t.Fatal("a list with an unknown alias resolved")
	}
	if got != nil {
		t.Errorf("a partial list was returned alongside the error: %v", got)
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error does not name the missing alias: %v", err)
	}
}

func TestRegistryAliasesAreSorted(t *testing.T) {
	registry := NewRegistryWith(map[string]Destination{
		"zeta":  newCheckedDestination("zeta", nil),
		"alpha": newCheckedDestination("alpha", nil),
		"mu":    newCheckedDestination("mu", nil),
	})
	if got, want := registry.Aliases(), []string{"alpha", "mu", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if got := NewRegistryWith(nil).Aliases(); len(got) != 0 {
		t.Errorf("an empty registry listed %v", got)
	}
}

func TestCheckAllPassesWhenEveryDestinationIsReachable(t *testing.T) {
	a, b := newCheckedDestination("a", nil), newCheckedDestination("b", nil)
	registry := NewRegistryWith(map[string]Destination{"a": a, "b": b})

	if err := registry.CheckAll(t.Context()); err != nil {
		t.Fatalf("healthy destinations failed the check: %v", err)
	}
	if a.checks.Load() != 1 || b.checks.Load() != 1 {
		t.Errorf("checks: a=%d b=%d, want each checked once", a.checks.Load(), b.checks.Load())
	}
}

// Every problem is reported at once, so the operator is not left fixing one
// account per restart.
func TestCheckAllReportsEveryFailureNotJustTheFirst(t *testing.T) {
	healthy := newCheckedDestination("healthy", nil)
	registry := NewRegistryWith(map[string]Destination{
		"healthy": healthy,
		"bucket":  newCheckedDestination("bucket", errors.New("zz: NoSuchBucket")),
		"keys":    newCheckedDestination("keys", errors.New("aa: InvalidAccessKeyId")),
	})

	err := registry.CheckAll(t.Context())
	if err == nil {
		t.Fatal("failing destinations passed the check")
	}
	msg := err.Error()
	first, second := strings.Index(msg, "aa: InvalidAccessKeyId"), strings.Index(msg, "zz: NoSuchBucket")
	if first < 0 || second < 0 {
		t.Fatalf("not every failure was reported: %v", err)
	}
	if first > second {
		t.Errorf("failures are not sorted, so the message changes from run to run: %v", err)
	}
	if healthy.checks.Load() != 1 {
		t.Error("a failure elsewhere stopped the healthy destination being checked")
	}
}

func TestCheckEachReportsTheOutcomePerAlias(t *testing.T) {
	broken := errors.New("access denied")
	registry := NewRegistryWith(map[string]Destination{
		"good": newCheckedDestination("good", nil),
		"bad":  newCheckedDestination("bad", broken),
	})

	results := registry.CheckEach(t.Context())
	if len(results) != 2 {
		t.Fatalf("got results for %d aliases, want 2: %v", len(results), results)
	}
	if err, ok := results["good"]; !ok || err != nil {
		t.Errorf("good: present=%v err=%v, want present and nil", ok, err)
	}
	if !errors.Is(results["bad"], broken) {
		t.Errorf("bad: got %v, want the destination's own error", results["bad"])
	}

	if empty := NewRegistryWith(nil).CheckEach(t.Context()); len(empty) != 0 {
		t.Errorf("an empty registry reported %v", empty)
	}
}

func TestNewBuildsTheClientTheProviderCallsFor(t *testing.T) {
	hermeticAWS(t)

	cases := []struct {
		name string
		cfg  config.Destination
		want string
	}{
		{"s3", s3Config("aws", "https://s3.invalid"), "*dest.S3Destination"},
		{"oci", func() config.Destination {
			c := s3Config("oracle", "https://oci.invalid")
			c.Provider = config.ProviderOCI
			return c
		}(), "*dest.S3Destination"},
		{"gdrive", driveConfig("drive"), "*dest.GDriveDestination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := New(t.Context(), tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := reflect.TypeOf(d).String(); got != tc.want {
				t.Errorf("built a %s, want a %s", got, tc.want)
			}
			if d.Alias() != tc.cfg.Alias {
				t.Errorf("alias %q, want %q", d.Alias(), tc.cfg.Alias)
			}
			if d.Info().Provider != string(tc.cfg.Provider) {
				t.Errorf("provider %q, want %q", d.Info().Provider, tc.cfg.Provider)
			}
		})
	}
}

func TestNewS3DestinationRecordsItsConfiguration(t *testing.T) {
	hermeticAWS(t)
	cfg := s3Config("offsite", "https://objects.invalid")
	cfg.StorageClass = "STANDARD_IA"
	cfg.Prefix = "feather/"

	d, err := NewS3Destination(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := Info{
		Alias: "offsite", Provider: "s3", Bucket: "archives", Region: "us-east-1",
		Endpoint: "https://objects.invalid", StorageClass: "STANDARD_IA", Prefix: "feather/",
	}
	if got := d.Info(); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestNewRegistryBuildsAClientPerAlias(t *testing.T) {
	hermeticAWS(t)

	registry, err := NewRegistry(t.Context(), map[string]config.Destination{
		"aws":   s3Config("aws", "https://s3.invalid"),
		"drive": driveConfig("drive"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Aliases(); !reflect.DeepEqual(got, []string{"aws", "drive"}) {
		t.Errorf("aliases: %v", got)
	}
}

func TestNewRegistryFailsOnAnUnusableAccount(t *testing.T) {
	bad := driveConfig("drive")
	bad.OAuthRefreshToken = ""

	registry, err := NewRegistry(t.Context(), map[string]config.Destination{"drive": bad})
	if err == nil {
		t.Fatal("a Drive account with no credentials was accepted")
	}
	if registry != nil {
		t.Error("a registry was returned alongside the error")
	}
	if !strings.Contains(err.Error(), "no Google Drive credentials") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// One bad account in an edit must leave the working set in place, not empty
// the registry and stop every job.
func TestReplaceKeepsThePreviousClientsWhenOneAccountFails(t *testing.T) {
	hermeticAWS(t)
	working := newCheckedDestination("working", nil)
	registry := NewRegistryWith(map[string]Destination{"working": working})

	bad := driveConfig("drive")
	bad.OAuthRefreshToken = ""
	err := registry.Replace(t.Context(), map[string]config.Destination{
		"fine":  s3Config("fine", "https://s3.invalid"),
		"drive": bad,
	})
	if err == nil {
		t.Fatal("Replace accepted an unusable account")
	}

	got, err := registry.Get("working")
	if err != nil || got != working {
		t.Errorf("the previous client was lost: %v, %v", got, err)
	}
	if aliases := registry.Aliases(); !reflect.DeepEqual(aliases, []string{"working"}) {
		t.Errorf("registry now holds %v", aliases)
	}
}

func TestReplaceSwapsTheWholeSet(t *testing.T) {
	hermeticAWS(t)
	registry := NewRegistryWith(map[string]Destination{"old": newCheckedDestination("old", nil)})

	if err := registry.Replace(t.Context(), map[string]config.Destination{
		"new": s3Config("new", "https://s3.invalid"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := registry.Aliases(); !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("after Replace the registry holds %v", got)
	}
	if _, err := registry.Get("old"); err == nil {
		t.Error("a removed alias still resolves")
	}
}

// Probe is how the UI tests credentials before saving them. It talks to the
// configured endpoint, so a local stand-in answers HeadBucket.
func TestProbeChecksTheBucketAtTheConfiguredEndpoint(t *testing.T) {
	hermeticAWS(t)

	var heads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/archives" {
			heads.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	t.Run("an existing bucket passes", func(t *testing.T) {
		if err := Probe(t.Context(), s3Config("local", server.URL)); err != nil {
			t.Fatalf("probe failed against a bucket that exists: %v", err)
		}
		if heads.Load() == 0 {
			t.Error("the probe passed without asking the endpoint")
		}
	})

	t.Run("a missing bucket fails permanently and says which", func(t *testing.T) {
		cfg := s3Config("local", server.URL)
		cfg.Bucket = "nope"

		err := Probe(t.Context(), cfg)
		if err == nil {
			t.Fatal("probe passed against a bucket that does not exist")
		}
		if !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("error does not name the destination and bucket: %v", err)
		}
		if retry.Retryable(err) {
			t.Errorf("a missing bucket was marked retryable: %v", err)
		}
	})
}

func TestProbeReportsAnAccountThatCannotBeBuilt(t *testing.T) {
	bad := driveConfig("drive")
	bad.OAuthRefreshToken = ""

	if err := Probe(t.Context(), bad); err == nil {
		t.Fatal("probe passed an account with no credentials")
	}
}

// --- helpers ----------------------------------------------------------------

// checkedDestination is a fakeDestination with its own alias and a Check
// outcome the test chooses.
type checkedDestination struct {
	fakeDestination
	alias    string
	checkErr error
	checks   atomic.Int32
}

func newCheckedDestination(alias string, checkErr error) *checkedDestination {
	return &checkedDestination{alias: alias, checkErr: checkErr}
}

func (c *checkedDestination) Alias() string { return c.alias }
func (c *checkedDestination) Info() Info    { return Info{Alias: c.alias} }
func (c *checkedDestination) Check(context.Context) error {
	c.checks.Add(1)
	return c.checkErr
}

func s3Config(alias, endpoint string) config.Destination {
	return config.Destination{
		Alias:           alias,
		Provider:        config.ProviderS3,
		Region:          "us-east-1",
		Bucket:          "archives",
		Endpoint:        endpoint,
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
	}
}

// driveConfig uses an OAuth refresh token, which builds a client without any
// network call; the token is only exchanged on the first request.
func driveConfig(alias string) config.Destination {
	return config.Destination{
		Alias:             alias,
		Provider:          config.ProviderGDrive,
		DriveFolderID:     "folder",
		OAuthClientID:     "client",
		OAuthClientSecret: "secret",
		OAuthRefreshToken: "refresh",
	}
}

// hermeticAWS stops the SDK reading the developer's own AWS profile or asking
// an instance metadata service for anything.
func hermeticAWS(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
}
