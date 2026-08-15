package dest

import (
	"context"
	"io"
)

// Destination is one place archives are written. Feather treats every provider
// the same way: put the object, then read its metadata back and confirm it
// matches what was sent. Only after that is the source data allowed to go.
type Destination interface {
	Alias() string
	Info() Info

	// Check verifies at startup that the credentials work and the bucket
	// exists, so a misconfigured destination fails before any data is read.
	Check(ctx context.Context) error

	// Key applies this destination's own prefix to a run-generated key, so the
	// same archive can be filed under a different layout per destination.
	Key(key string) string

	// Upload stores body at key. size is the exact byte length of body.
	Upload(ctx context.Context, key string, body io.Reader, size int64, meta Metadata) (ETag string, err error)

	// Verify re-reads the stored object and confirms its size and recorded
	// checksum. An error here blocks deletion of the source documents.
	Verify(ctx context.Context, key string, wantSize int64, wantSHA256 string) error
}

// Info describes a destination for the catalogue and for logs.
type Info struct {
	Alias        string
	Provider     string
	Bucket       string
	Region       string
	Endpoint     string
	StorageClass string
	Prefix       string
}

// Metadata travels with the object. The checksum in particular is what makes
// verification meaningful — ETag is unusable for that on multipart uploads.
type Metadata struct {
	SHA256     string
	DocCount   int64
	Config     string
	RunID      string
	Collection string
}
