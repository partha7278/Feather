package bundle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/store"
)

// A bundle is a file that has left the machine: mailed, committed, copied to a
// laptop. Everything in it is attacker-controlled until proven otherwise, and
// importing it writes credentials and retention configs — the things that
// decide what feather deletes and where it sends the data.

const attackerEndpoint = "https://attacker.example"

// The credentials in a bundle are sealed, but the fields beside them are not.
// Repointing the exported S3 destination at another endpoint must not survive
// an import with the right passphrase: that would hand the real secret, and
// every archive written afterwards, to whoever edited the file.
func TestTamperingWithASealedBundleIsDetected(t *testing.T) {
	ctx := context.Background()
	source := newInstance(t)
	seed(t, source)

	exported, err := Export(ctx, source, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	for i := range exported.Destinations {
		if exported.Destinations[i].Alias == "cold" {
			exported.Destinations[i].Endpoint = attackerEndpoint
			exported.Destinations[i].ForcePathStyle = true
		}
	}
	encoded, err := exported.Encode()
	if err != nil {
		t.Fatal(err)
	}

	target := newInstance(t)
	decoded, err := Decode(encoded)
	if err != nil {
		return // refusing the file outright is secure
	}
	if _, err := decoded.Apply(ctx, target, ImportOptions{Passphrase: passphrase}); err != nil {
		return
	}
	if cold := destinationNamed(t, target, "cold"); cold != nil &&
		cold.Endpoint == attackerEndpoint && cold.SecretAccessKey == "very-secret-key" {
		t.Fatal("an edited endpoint was imported alongside the genuine secret: the bundle's " +
			"plaintext fields are not covered by any integrity check")
	}
}

// A credential-free bundle is the form meant for version control and for
// handing to somebody else. Imported with Overwrite, it must not be able to
// move an existing destination to a new endpoint while the stored secret stays
// attached to it.
func TestACredentialFreeBundleCannotRepointAStoredSecret(t *testing.T) {
	ctx := context.Background()
	target := newInstance(t)
	seedAccounts(t, target)

	crafted := &Bundle{
		Feather: Header{Version: Version},
		Destinations: []Destination{{
			Alias: "cold", Provider: "s3", Region: "ap-south-1", Bucket: "archive",
			AccessKeyID: "AKIA", Prefix: "feather", Enabled: true,
			Endpoint: attackerEndpoint, ForcePathStyle: true,
		}},
	}
	encoded, err := crafted.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decoded.Apply(ctx, target, ImportOptions{
		Overwrite: true, Sections: Sections{Destinations: true},
	}); err != nil {
		return
	}

	cold := destinationNamed(t, target, "cold")
	if cold != nil && cold.Endpoint == attackerEndpoint && cold.SecretAccessKey == "very-secret-key" {
		t.Fatal("a bundle with no credentials and no passphrase moved the stored S3 secret to a new endpoint")
	}
}

// Without an envelope there is nothing to open sealed values with. A bundle
// that simply writes credentials in the clear must not have them stored — the
// format only ever carries them sealed.
func TestPlaintextCredentialsWithoutAnEnvelopeAreNotImported(t *testing.T) {
	ctx := context.Background()
	target := newInstance(t)

	crafted := &Bundle{
		Feather: Header{Version: Version},
		MongoSources: []MongoSource{{
			Alias: "smuggled", URI: "mongodb://attacker:pw@attacker.example:27017", Enabled: true,
		}},
		Destinations: []Destination{{
			Alias: "smuggled", Provider: "s3", Region: "us-east-1", Bucket: "exfil",
			AccessKeyID: "AKIA", SecretAccessKey: "plaintext-secret", Enabled: true,
		}},
	}
	result, err := crafted.Apply(ctx, target, ImportOptions{Passphrase: passphrase})
	if err != nil {
		return
	}
	if result.MongoSources.Created+result.Destinations.Created != 0 {
		t.Fatalf("plaintext credentials were imported: %s", result.Summary())
	}
	sources, err := target.Store.ListMongoSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Errorf("a MongoDB account was created from a plaintext URI: %+v", sources)
	}
	if destinationNamed(t, target, "smuggled") != nil {
		t.Error("a destination was created from a plaintext secret")
	}
}

// One sealed value is edited. The envelope still opens, so the passphrase is
// right — the damaged entry alone is skipped and nothing garbled is stored.
func TestATamperedSealedValueIsSkippedNotStored(t *testing.T) {
	ctx := context.Background()
	source := newInstance(t)
	seedAccounts(t, source)

	exported, err := Export(ctx, source, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	for i := range exported.Destinations {
		if exported.Destinations[i].Alias != "cold" {
			continue
		}
		sealed, err := base64.StdEncoding.DecodeString(exported.Destinations[i].SecretAccessKey)
		if err != nil {
			t.Fatal(err)
		}
		sealed[len(sealed)-1] ^= 0x01
		exported.Destinations[i].SecretAccessKey = base64.StdEncoding.EncodeToString(sealed)
	}

	target := newInstance(t)
	result, err := exported.Apply(ctx, target, ImportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	if destinationNamed(t, target, "cold") != nil {
		t.Fatal("a destination with a tampered secret was imported")
	}
	if destinationNamed(t, target, "drive") == nil {
		t.Error("the untouched destination was not imported alongside the damaged one")
	}
	if !strings.Contains(strings.Join(result.Notes, "\n"), "cold") {
		t.Errorf("the damaged entry was not reported: %v", result.Notes)
	}
}

// Sealed values swapped between fields or entries open fine under the same
// key. The result must at worst be a confused account, never a secret of one
// destination attached to a different, attacker-chosen one.
func TestASealedSecretMovedToAnotherEntryIsRefused(t *testing.T) {
	ctx := context.Background()
	source := newInstance(t)
	seedAccounts(t, source)

	exported, err := Export(ctx, source, ExportOptions{Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	var stolen string
	for _, d := range exported.Destinations {
		if d.Alias == "cold" {
			stolen = d.SecretAccessKey
		}
	}
	exported.Destinations = []Destination{{
		Alias: "mirror", Provider: "s3", Region: "us-east-1", Bucket: "exfil", AccessKeyID: "AKIA",
		Endpoint: attackerEndpoint, SecretAccessKey: stolen, Enabled: true,
	}}

	target := newInstance(t)
	if _, err := exported.Apply(ctx, target, ImportOptions{Passphrase: passphrase}); err != nil {
		return
	}
	if mirror := destinationNamed(t, target, "mirror"); mirror != nil && mirror.SecretAccessKey == "very-secret-key" {
		t.Fatal("cold's sealed secret, transplanted into another entry, was accepted: sealed values " +
			"are not bound to the field and alias they were exported for")
	}
}

// Fields the format does not have — a record ID, say — are ignored rather than
// used to aim an import at an existing row under another alias.
func TestUnknownFieldsCannotAimAnImportAtAnotherRecord(t *testing.T) {
	ctx := context.Background()
	target := newInstance(t)
	seedAccounts(t, target)
	cold := destinationNamed(t, target, "cold")
	if cold == nil {
		t.Fatal("fixture: no cold destination")
	}

	raw := fmt.Sprintf(`{
		"feather": {"version": %d},
		"destinations": [{
			"id": %q, "ID": %q, "alias": "renamed", "provider": "s3",
			"region": "us-east-1", "bucket": "exfil", "enabled": true
		}],
		"mongoSources": [{"id": "x", "alias": "prod", "uri": ""}]
	}`, Version, cold.ID, cold.ID)
	decoded, err := Decode([]byte(raw))
	if err != nil {
		return
	}
	if _, err := decoded.Apply(ctx, target, ImportOptions{Sections: Sections{Destinations: true}}); err != nil {
		return
	}

	after := destinationNamed(t, target, "cold")
	if after == nil || after.Bucket != "archive" || after.SecretAccessKey != "very-secret-key" {
		t.Fatalf("an injected id field rewrote the existing destination: %+v", after)
	}
}

// The scrypt cost comes from the file. The existing bound stops N alone from
// running away, but memory is 128·N·r bytes, so N and r at their caps together
// still ask for gigabytes — on an endpoint any administrator can post to.
func TestScryptParametersAcceptedFromAFileAreBoundedInMemory(t *testing.T) {
	const ceiling = 1 << 30 // the existing test already calls a 1 GB derivation absurd
	for _, p := range []struct{ n, r, p int }{
		{1 << 20, 16, 4},
		{1 << 20, 16, 1},
		{1 << 19, 16, 1},
		{1 << 20, 8, 1},
	} {
		if err := checkScryptParams(p.n, p.r, p.p); err != nil {
			continue
		}
		if memory := int64(128) * int64(p.n) * int64(p.r); memory > ceiling {
			t.Errorf("scrypt N=%d r=%d p=%d is accepted from a bundle and needs %d MiB",
				p.n, p.r, p.p, memory>>20)
		}
	}
}

// Only versions this build knows are accepted. Zero is "not a bundle"; a
// negative version is no more a bundle than zero is.
func TestDecodeRejectsVersionsThatDoNotExist(t *testing.T) {
	for _, version := range []string{"0", "-1", "-2147483648", "2", "1.5", `"1"`, "null"} {
		body := `{"feather":{"version":` + version + `}}`
		if b, err := Decode([]byte(body)); err == nil {
			t.Errorf("version %s was accepted (decoded as %d)", version, b.Feather.Version)
		}
	}
}

// A refusal on parse is named as such, so the caller can tell a hostile or
// broken file from a real import failure.
func TestDecodeNamesMalformedFilesAsNotABundle(t *testing.T) {
	for _, body := range []string{
		``, `null`, `{"feather":null}`, `{"feather":{"version":1}`, `{"feather":{"version":"x"}}`,
		strings.Repeat("[", 100_000), "\xff\xfe{}",
	} {
		_, err := Decode([]byte(body))
		if err == nil {
			t.Errorf("%.40q was accepted", body)
			continue
		}
		if !errors.Is(err, ErrNotABundle) {
			t.Errorf("%.40q: error is not ErrNotABundle: %v", body, err)
		}
	}
}

// The instance name is whatever the exporting installation called itself; the
// download name built from it must not be a path.
func TestFileNameCannotBeAPath(t *testing.T) {
	for _, instance := range []string{
		"../../etc/cron.d/x", `..\..\Windows\System32`, "/absolute", "..", "a\x00b", "name\r\nX-Header: 1",
	} {
		b := &Bundle{Feather: Header{Instance: instance}}
		name := b.FileName()
		if strings.ContainsAny(name, "/\\\x00\r\n") || strings.Contains(name, "..") || filepath.Base(name) != name {
			t.Errorf("instance %q produced the download name %q", instance, name)
		}
	}
}

// FuzzDecode holds that any bytes either decode to a bundle of a version this
// build understands or are refused — never a panic. The seeds run under plain
// `go test`.
func FuzzDecode(f *testing.F) {
	full := &Bundle{
		Feather:      Header{Version: Version, Secrets: &Envelope{KDF: "scrypt", N: 1 << 15, R: 8, P: 1, Salt: "AAAA", Check: "AAAA"}},
		MongoSources: []MongoSource{{Alias: "a", URI: "!!"}},
		Destinations: []Destination{{Alias: "d", Provider: "s3"}},
		Configs:      nil,
	}
	encoded, err := json.Marshal(full)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte(`{"feather":{"version":1},"configs":[null,{"name":"x"}]}`))
	f.Add([]byte(`{"feather":{"version":1},"runs":[{}]}`))
	f.Add([]byte(`{"feather":{"version":99}}`))
	f.Add([]byte(`{"feather":{"version":1e400}}`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := Decode(data)
		if err != nil {
			return
		}
		if b.Feather.Version > Version || b.Feather.Version == 0 {
			t.Fatalf("accepted version %d", b.Feather.Version)
		}
		_ = b.FileName()
		_ = b.HasSecrets()
		if _, err := b.Encode(); err != nil {
			t.Fatalf("a decoded bundle does not re-encode: %v", err)
		}
	})
}

func destinationNamed(t *testing.T, a *app.App, alias string) *store.DestinationRecord {
	t.Helper()
	destinations, err := a.Store.ListDestinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range destinations {
		if destinations[i].Alias == alias {
			return &destinations[i]
		}
	}
	return nil
}
