//go:build integration

// Package e2e holds feather's end-to-end suite: real MongoDB, real object
// storage, a real run, and a real restore.
//
// Everything here is behind the `integration` build tag, so `go test ./...`
// never touches it. Run it with `make test-integration` against the rig in
// testdata/docker-compose.yml.
//
// The suite exists to check the one promise the unit tests cannot: that when
// feather says a document is safely archived, the bytes are actually in the
// bucket, they hash to what the catalogue claims, and they restore to the same
// documents that were deleted.
package e2e

import (
	"os"
	"testing"
)

// Rig describes where the local verification rig is listening. The values match
// testdata/docker-compose.yml and testdata/.env.rig.
type Rig struct {
	MongoURI    string
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	Buckets     []string
}

// requireRig returns the rig's addresses, skipping the test when it is not up.
// Skipping rather than failing is deliberate: `make test-integration` should be
// safe to run on a laptop with no Docker, and CI starts the rig explicitly.
func requireRig(t *testing.T) Rig {
	t.Helper()

	uri := envOr("FEATHER_TEST_MONGO_URI", "")
	endpoint := envOr("FEATHER_TEST_S3_ENDPOINT", "")
	if uri == "" || endpoint == "" {
		t.Skip("integration rig not configured — run `make rig-up` and set " +
			"FEATHER_TEST_MONGO_URI and FEATHER_TEST_S3_ENDPOINT")
	}

	return Rig{
		MongoURI:    uri,
		S3Endpoint:  endpoint,
		S3AccessKey: envOr("FEATHER_TEST_S3_ACCESS_KEY", "featherkey"),
		S3SecretKey: envOr("FEATHER_TEST_S3_SECRET_KEY", "feathersecret"),
		Buckets:     []string{"feather-primary", "feather-backup", "feather-cold"},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
