package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Passwords are not always URL-encoded by the time they reach an error
// message, and the characters that most often break a naive redactor are the
// ones operators are told to put in passwords. Each case must lose its secret
// and keep its host.
func TestRedactSecretsHandlesAwkwardPasswords(t *testing.T) {
	cases := []struct {
		name, input, secret, host string
	}{
		{"url-encoded at sign", "dial mongodb://app:p%40ss%3Aw0rd@db.example.com:27017/x", "p%40ss%3Aw0rd", "db.example.com:27017"},
		{"percent-encoded slash", "dial mongodb://app:a%2Fb%2Fc@db.example.com:27017", "a%2Fb%2Fc", "db.example.com:27017"},
		{"hash and bang", "dial mongodb://app:h#sh!x@db.example.com:27017", "h#sh!x", "db.example.com:27017"},
		{"question mark", "dial mongodb://app:wh?t@db.example.com:27017", "wh?t", "db.example.com:27017"},
		{"srv with options", "mongodb+srv://app:Zx9-Qq_~@c0.mongodb.net/?retryWrites=true", "Zx9-Qq_~", "c0.mongodb.net"},
		{"multiple hosts", "mongodb://app:s3cr3t@h1:27017,h2:27017,h3:27017/?replicaSet=rs0", "s3cr3t", "h1:27017,h2:27017"},
		{"quoted in an error", `error parsing uri "mongodb://app:quoted-pw@db:27017"`, "quoted-pw", "db:27017"},
		{"unencoded at sign in the password", "dial mongodb://app:p@ss@db.example.com:27017", "p@ss", "db.example.com:27017"},
		{"unencoded slash in the password", "dial mongodb://app:pa/ss@db.example.com:27017", "pa/ss", "db.example.com:27017"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.input)
			if strings.Contains(got, tc.secret) {
				t.Errorf("the password survived redaction: %s", got)
			}
			if !strings.Contains(got, tc.host) {
				t.Errorf("redaction lost the host: %s", got)
			}
		})
	}
}

// Credentials do not only travel in userinfo. The MongoDB URI carries AWS
// session tokens and certificate passwords in its options, SDKs put keys in
// query strings, and OAuth endpoints answer with JSON bodies.
func TestRedactSecretsFindsCredentialsOutsideUserinfo(t *testing.T) {
	cases := []struct {
		name, input, secret string
	}{
		{"aws session token in authMechanismProperties", "mongodb://db/?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:FwoGZXIvYXdzEBEaDH", "FwoGZXIvYXdzEBEaDH"},
		{"tls certificate key password", "mongodb://db/?tls=true&tlsCertificateKeyFilePassword=c3rtpw", "c3rtpw"},
		{"password as a query parameter", "https://hook.example.com/x?password=qp-secret&y=1", "qp-secret"},
		{"oauth client secret in a form body", "body: client_id=abc&client_secret=GOCSPX-shh&grant_type=refresh_token", "GOCSPX-shh"},
		{"refresh token in a form body", "refresh_token=1//0gREFRESH&client_id=x", "1//0gREFRESH"},
		{"https userinfo", "Post https://bot:tok3n@hooks.example.com/notify: EOF", "tok3n"},
		{"json password field", `upstream said {"password":"json-pw","user":"x"}`, "json-pw"},
		{"json secret access key field", `{"secretAccessKey": "wJalrXUtnFEMI/K7MDENG", "region":"x"}`, "wJalrXUtnFEMI/K7MDENG"},
		{"json refresh token field", `{"refresh_token":"1//04-refresh"}`, "1//04-refresh"},
		{"authorization bearer header", "request headers: Authorization: Bearer ya29.bearer-secret", "ya29.bearer-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactSecrets(tc.input); strings.Contains(got, tc.secret) {
				t.Errorf("the credential survived redaction: %s", got)
			}
		})
	}
}

// RedactMongoURI is what the UI shows as the connection string preview. A
// secret in the options must be hidden as surely as one in the userinfo.
func TestRedactMongoURIHidesSecretsInTheOptions(t *testing.T) {
	cases := map[string]string{
		"mongodb://app:pw@db/?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:FwoGZXIvSESSION": "FwoGZXIvSESSION",
		"mongodb://db/?tls=true&tlsCertificateKeyFilePassword=c3rt-pw":                                             "c3rt-pw",
		"mongodb://app:p%40ss%2Fword@db:27017":                                                                     "p%40ss%2Fword",
		"mongodb://app:p@ss@db:27017":                                                                              "ss@db",
	}
	for input, secret := range cases {
		got := RedactMongoURI(input)
		if strings.Contains(got, secret) {
			t.Errorf("%q redacted to %q, which still contains %q", input, got, secret)
		}
	}

	// A username-only URI (x.509, Kerberos) has no password to hide and must be
	// left readable.
	if got := RedactMongoURI("mongodb://CN=client@db/?authMechanism=MONGODB-X509"); !strings.Contains(got, "db") {
		t.Errorf("a password-less URI lost its host: %s", got)
	}
}

// Nothing that holds a credential may print it when it is formatted with %v or
// encoded as JSON. These types travel through logs, error wrapping and the
// web layer, and the first careless slog.Any on one should not be a breach.
func TestCredentialBearingTypesDoNotPrintTheirSecrets(t *testing.T) {
	const (
		password  = "uri-password-123"
		secretKey = "secret-access-key-abc"
		session   = "session-token-def"
		oauth     = "oauth-client-secret-ghi"
		refresh   = "refresh-token-jkl"
		privKey   = "-----BEGIN PRIVATE KEY-----MIIEsecret"
	)
	source := MongoSource{Alias: "prod", URI: "mongodb://app:" + password + "@db:27017"}
	dest := Destination{
		Alias: "cold", Provider: ProviderS3, Bucket: "b", Region: "r",
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: secretKey, SessionToken: session,
		OAuthClientSecret: oauth, OAuthRefreshToken: refresh,
		ServiceAccountJSON: `{"private_key":"` + privKey + `"}`,
	}
	secrets := []string{password, secretKey, session, oauth, refresh, privKey}

	renderings := map[string]func() string{
		"source %v":  func() string { return fmt.Sprintf("%v", source) },
		"source %+v": func() string { return fmt.Sprintf("%+v", source) },
		"dest %v":    func() string { return fmt.Sprintf("%v", dest) },
		"dest %+v":   func() string { return fmt.Sprintf("%+v", dest) },
		"source json": func() string {
			b, _ := json.Marshal(source)
			return string(b)
		},
		"dest json": func() string {
			b, _ := json.Marshal(dest)
			return string(b)
		},
	}
	for name, render := range renderings {
		t.Run(name, func(t *testing.T) {
			out := render()
			for _, s := range secrets {
				if strings.Contains(out, s) {
					t.Errorf("rendering exposes %q: %s", s, out)
				}
			}
		})
	}
}

// A retention config is the one object that is meant to be shown, logged and
// fingerprinted freely. It names accounts by alias and must never carry a
// credential — even when the filter happens to contain text that looks like one.
func TestJobConfigCarriesAliasesNotCredentials(t *testing.T) {
	job := validPrepareJob()
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"uri", "URI", "secretAccessKey", "SecretAccessKey", "password", "refreshToken"} {
		if strings.Contains(string(encoded), `"`+field+`"`) {
			t.Errorf("a job config serialises a %q field: %s", field, encoded)
		}
	}
}

// The config name becomes a spool directory (filepath.Join(SpoolDir, name)) and
// the {config} segment of every object key. It must not be able to walk out of
// either.
func TestAConfigNameCannotEscapeItsDirectory(t *testing.T) {
	hostile := []string{
		"..", ".", "../etc", "a/../../b", "/abs", `a\b`, "..\\x", "a\x00b",
		"a\nb", ".hidden", "-flag", "a b", "a%2f..", strings.Repeat("a", 65),
	}
	for _, name := range hostile {
		t.Run(name, func(t *testing.T) {
			job := validPrepareJob()
			job.Name = name
			if err := job.Prepare(); err == nil {
				t.Fatalf("accepted the config name %q", name)
			}
		})
	}
}

// The template validator refuses ".." and "/" tricks in the template itself;
// a variable substituted into it must not be a way around that. MongoDB permits
// "/", "\\" and ".." in collection names, Prepare only checks the collection is
// non-empty, and a Google Drive destination turns every segment into a real
// folder.
func TestRenderedKeysStayInsideThePrefixWhateverTheVariablesHold(t *testing.T) {
	vars := []struct {
		name string
		mut  func(*KeyVars)
	}{
		{"collection with parent segments", func(v *KeyVars) { v.Collection = "../../../escape" }},
		{"collection that is just dots", func(v *KeyVars) { v.Collection = ".." }},
		{"collection with a backslash", func(v *KeyVars) { v.Collection = `..\..\win` }},
		{"collection with a doubled slash", func(v *KeyVars) { v.Collection = "a//b" }},
	}
	const template = "feather/{db}/{collection}/{runId}-{seq}{ext}"
	if err := ValidateKeyTemplate(template); err != nil {
		t.Fatal(err)
	}
	for _, tc := range vars {
		t.Run(tc.name, func(t *testing.T) {
			v := KeyVars{Database: "shop", Collection: "orders", Config: "c", RunID: "r", Seq: 1, Extension: ".ndjson.gz", At: time.Now()}
			tc.mut(&v)
			key := RenderKey(template, v)
			if strings.HasPrefix(key, "/") || strings.Contains(key, "//") || strings.ContainsAny(key, "\\\x00") {
				t.Errorf("rendered key %q has a shape the template validator forbids", key)
			}
			for _, segment := range strings.Split(key, "/") {
				if segment == ".." || segment == "." {
					t.Errorf("rendered key %q contains a %q segment", key, segment)
				}
			}
		})
	}
}

// Time tokens are substituted textually. Whatever an operator writes, the only
// thing that can come out of a token is a timestamp, and a token placed where
// it would produce something other than valid JSON is refused rather than sent
// to MongoDB in an unexpected shape.
func TestTimeTokensCannotInjectStructureIntoAFilter(t *testing.T) {
	now := time.Date(2026, 5, 14, 2, 30, 0, 0, time.UTC)
	refused := []string{
		`{"at": {{now}} }`,
		`{"at": {"$lt": {{now-1d}}}}`,
		`{"at":"x"} {{now}}`,
		`{"at":"{{now-99999999999999999999h}}"}`,
	}
	for _, raw := range refused {
		if got, err := ResolveTimeTokens(json.RawMessage(raw), now); err == nil {
			t.Errorf("%s resolved to %s instead of being refused", raw, got)
		}
	}

	// A token inside a string stays inside it.
	got, err := ResolveTimeTokens(json.RawMessage(`{"at":{"$lt":{"$date":"{{now-90d}}"}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("resolved filter changed shape: %s (%v)", got, err)
	}
	if decoded["at"]["$lt"]["$date"] != "2026-02-13T02:30:00.000Z" {
		t.Errorf("unexpected resolution %s", got)
	}
}

// A config file is parsed with the standard decoder. Pathological input must
// fail cleanly rather than exhausting the stack.
func TestLoadJobRefusesPathologicallyNestedJSON(t *testing.T) {
	dir := t.TempDir()
	depth := 200_000
	filter := strings.Repeat(`{"a":`, depth) + `1` + strings.Repeat(`}`, depth)
	body := `{"name":"deep","source":{"mongoSource":"prod","database":"d","collection":"c","filter":` + filter + `},` +
		`"destinations":["cold"],"schedule":{"every":"1d"},"output":{"keyTemplate":"a/{runId}{ext}"}}`
	path := writeJobFile(t, dir, "deep.json", body)

	if _, err := LoadJob(path, loadCatalog); err == nil {
		t.Fatal("a filter nested 200,000 levels deep was accepted")
	}
}

// The strict loader is what keeps a misspelt safety field — "verifyBeforDelete"
// — from being silently ignored and leaving the dangerous default in place.
func TestStrictLoadRejectsMisspeltSafetyFields(t *testing.T) {
	dir := t.TempDir()
	body := jobJSON("typo", "prod", `"retention":{"deleteAfterArchive":true,"verifyBeforDelete":false}`)
	path := writeJobFile(t, dir, "typo.json", body)
	_, err := LoadJob(path, loadCatalog)
	if err == nil || !strings.Contains(err.Error(), "verifyBeforDelete") {
		t.Fatalf("a misspelt retention field was not reported: %v", err)
	}
}

// The legacy importer reads an old .env file without touching the process
// environment, so values in it cannot change how the running daemon behaves —
// including the master key and the paths it trusts.
func TestLegacyImportDoesNotLeakIntoTheProcessEnvironment(t *testing.T) {
	unsetEnv(t, "FEATHER_MASTER_KEY")
	unsetEnv(t, "FEATHER_MASTER_KEY_FILE")
	path := filepath.Join(t.TempDir(), "old.env")
	content := "MONGO_URI=mongodb://a:b@db\nFEATHER_MASTER_KEY=" + strings.Repeat("ab", 32) + "\nFEATHER_MASTER_KEY_FILE=/tmp/evil.key\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLegacyEnv(path); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"FEATHER_MASTER_KEY", "FEATHER_MASTER_KEY_FILE", "MONGO_URI"} {
		if v, ok := os.LookupEnv(key); ok {
			t.Errorf("importing set %s=%q in the process environment", key, v)
		}
	}
}

// A legacy import that fails on a destination must name the alias, not repeat
// the secret key that was supplied for it.
func TestLegacyImportErrorsDoNotRepeatSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.env")
	content := "FEATHER_DESTINATIONS=bad\nDEST_BAD_PROVIDER=oci\nDEST_BAD_BUCKET=b\nDEST_BAD_REGION=r\n" +
		"DEST_BAD_SECRET_ACCESS_KEY=legacy-secret-xyz\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadLegacyEnv(path)
	if err == nil {
		t.Fatal("an incomplete Oracle destination was imported")
	}
	if strings.Contains(err.Error(), "legacy-secret-xyz") {
		t.Errorf("the import error repeats the secret: %v", err)
	}
}

// Destination validation errors are shown in the browser. None of them may
// quote the credential that was submitted.
func TestValidateDestinationErrorsDoNotQuoteCredentials(t *testing.T) {
	cases := []Destination{
		{Alias: "a", Provider: ProviderS3, Bucket: "b", Region: "r", Endpoint: "https://e", SecretAccessKey: "only-secret-1"},
		{Alias: "a", Provider: ProviderGDrive, ServiceAccountJSON: `{"private_key":"pk-secret-2"`, DriveFolderID: "f"},
		{Alias: "a", Provider: ProviderGDrive, OAuthClientSecret: "client-secret-3"},
		{Alias: "a", Provider: ProviderGDrive, ServiceAccountJSON: `{"k":"sa-secret-4"}`, OAuthRefreshToken: "rt-secret-5"},
	}
	for i, d := range cases {
		err := ValidateDestination(d)
		if err == nil {
			t.Errorf("case %d: accepted an invalid destination", i)
			continue
		}
		for _, s := range []string{"only-secret-1", "pk-secret-2", "client-secret-3", "sa-secret-4", "rt-secret-5"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("case %d: error quotes %q: %v", i, s, err)
			}
		}
	}
}

// RedactSecretsFromError is applied to wrapped errors; the whole chain's text
// must be cleaned, not just the outermost layer.
func TestRedactSecretsFromErrorCleansAWrappedChain(t *testing.T) {
	inner := errors.New("parse mongodb://u:deep-pw@h:1")
	outer := errors.Join(errors.New("connect"), inner)
	if got := RedactSecretsFromError(outer); strings.Contains(got, "deep-pw") {
		t.Errorf("the wrapped password survived: %s", got)
	}
}
