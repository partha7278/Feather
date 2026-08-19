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
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
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
//
// Skipping rather than failing is deliberate: `make test-integration` has to be
// safe to run on a laptop with no Docker. Configuration alone is not enough to
// decide that — the Makefile supplies the rig's addresses, so the variables are
// always set and "configured" would mean "always run". What settles it is
// whether anything is actually listening.
//
// CI is the exception: there the rig is started explicitly, so a rig that is
// not up is a failure rather than a reason to quietly pass. Set
// FEATHER_TEST_REQUIRE_RIG=1 to turn the skip into a failure.
func requireRig(t *testing.T) Rig {
	t.Helper()

	rig := Rig{
		MongoURI:    envOr("FEATHER_TEST_MONGO_URI", ""),
		S3Endpoint:  envOr("FEATHER_TEST_S3_ENDPOINT", ""),
		S3AccessKey: envOr("FEATHER_TEST_S3_ACCESS_KEY", "featherkey"),
		S3SecretKey: envOr("FEATHER_TEST_S3_SECRET_KEY", "feathersecret"),
		Buckets:     []string{"feather-primary", "feather-backup", "feather-cold"},
	}

	if rig.MongoURI == "" || rig.S3Endpoint == "" {
		unavailable(t, "FEATHER_TEST_MONGO_URI and FEATHER_TEST_S3_ENDPOINT are not set")
	}
	for _, target := range []struct{ what, address string }{
		{"MongoDB", hostPort(rig.MongoURI)},
		{"object storage", hostPort(rig.S3Endpoint)},
	} {
		conn, err := net.DialTimeout("tcp", target.address, 2*time.Second)
		if err != nil {
			unavailable(t, fmt.Sprintf("nothing is listening for %s on %s", target.what, target.address))
		}
		_ = conn.Close()
	}

	return rig
}

// unavailable skips, or fails when the caller insisted the rig be there.
func unavailable(t *testing.T, why string) {
	t.Helper()
	message := "integration rig is not up (" + why + ") — run `make rig-up`"
	if envOr("FEATHER_TEST_REQUIRE_RIG", "") != "" {
		t.Fatal(message + "; FEATHER_TEST_REQUIRE_RIG is set, so this is a failure rather than a skip")
	}
	t.Skip(message)
}

// hostPort reduces a URI to the address to dial, defaulting the port by scheme.
func hostPort(raw string) string {
	trimmed := raw
	if i := strings.Index(trimmed, "://"); i >= 0 {
		trimmed = trimmed[i+3:]
	}
	if i := strings.IndexAny(trimmed, "/?"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if i := strings.LastIndex(trimmed, "@"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if strings.Contains(trimmed, ":") {
		return trimmed
	}
	if strings.HasPrefix(raw, "https://") {
		return trimmed + ":443"
	}
	if strings.HasPrefix(raw, "mongodb://") {
		return trimmed + ":27017"
	}
	return trimmed + ":80"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
