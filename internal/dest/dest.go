package dest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/unifers/feather/internal/retry"
)

// Destination is one place archives are written. Feather treats every provider
// the same way: put the object, then read the whole thing back and confirm it
// hashes to what was sent. Only after that is the source data allowed to go.
type Destination interface {
	Alias() string
	Info() Info

	// Check verifies at startup that the credentials work and the bucket
	// exists, so a misconfigured destination fails before any data is read.
	Check(ctx context.Context) error

	// Key applies this destination's own prefix to a run-generated key, so the
	// same archive can be filed under a different layout per destination.
	Key(key string) string

	// Upload stores body at key. size is the exact byte length of body. The
	// returned handle is whatever this provider uses to name the stored object
	// afterwards — an ETag for S3, a file ID for Drive.
	Upload(ctx context.Context, key string, body io.Reader, size int64, meta Metadata) (handle string, err error)

	// Open streams a stored object back. The caller closes the reader. handle
	// is what Upload returned, and may be empty for an object written by an
	// older run — a provider that can use it should prefer it over key, since
	// a handle is stable against renames and eventually-consistent listings.
	Open(ctx context.Context, key, handle string) (io.ReadCloser, error)

	// Verify re-reads the stored object end to end and confirms it is byte for
	// byte what was uploaded. An error here blocks deletion of the source
	// documents, so it must fail closed: anything it cannot positively confirm
	// is an error, never a pass.
	Verify(ctx context.Context, obj StoredObject) error
}

// StoredObject names a copy that has just been written and states what it has
// to turn out to be.
type StoredObject struct {
	// Key is the object key as given to Upload, prefix already applied.
	Key string
	// Handle is what Upload returned. Empty is legal.
	Handle string
	// Size is the exact byte length that was uploaded.
	Size int64
	// SHA256 is the hex digest computed over the bytes as they were written to
	// the spool file, which is the source of truth for this comparison.
	SHA256 string
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

// Metadata travels with the object. The checksum in particular is what lets a
// later scrub confirm a copy without feather's catalogue to hand — ETag is
// unusable for that on multipart uploads.
type Metadata struct {
	SHA256     string
	DocCount   int64
	Config     string
	RunID      string
	Collection string
}

// VerifyByReading downloads the object and hashes every byte of it.
//
// This is the only check feather trusts. Comparing a length proves nothing
// about content, and comparing metadata that feather itself supplied proves
// nothing at all — it is feather's own value handed back. Reading the bytes and
// hashing them is the one test that fails when the stored copy is not the
// archive, which is the question being asked before documents are deleted.
//
// The cost is one download per copy per batch. That is deliberate: it is the
// price of the guarantee, and it is paid once, against a compressed file.
func VerifyByReading(ctx context.Context, d Destination, obj StoredObject) error {
	if obj.SHA256 == "" {
		// Refusing is the only safe answer. A missing expected digest means
		// there is nothing to compare against, and "nothing to compare" must
		// never read as "matches".
		return retry.Permanent(fmt.Errorf("verifying %s (%s): no expected checksum to compare against", d.Alias(), obj.Key))
	}

	body, err := d.Open(ctx, obj.Key, obj.Handle)
	if err != nil {
		return fmt.Errorf("verifying %s (%s): re-reading the stored copy: %w", d.Alias(), obj.Key, err)
	}
	defer func() { _ = body.Close() }()

	hash := sha256.New()
	read, err := io.Copy(hash, body)
	if err != nil {
		return fmt.Errorf("verifying %s (%s): reading the stored copy: %w", d.Alias(), obj.Key, err)
	}

	// A copy that read cleanly and came out wrong will come out wrong again:
	// the bytes in the bucket are the bytes in the bucket. Marking these
	// permanent stops feather re-downloading a large object two more times to
	// reach the same verdict, and reports the real problem immediately.
	if read != obj.Size {
		return retry.Permanent(fmt.Errorf("verifying %s (%s): stored copy is %d bytes, expected %d",
			d.Alias(), obj.Key, read, obj.Size))
	}
	if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, obj.SHA256) {
		return retry.Permanent(fmt.Errorf("verifying %s (%s): stored copy hashes to %s, expected %s",
			d.Alias(), obj.Key, got, obj.SHA256))
	}
	return nil
}
