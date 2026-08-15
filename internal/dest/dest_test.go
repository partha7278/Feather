package dest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// VerifyByReading is the check that stands between an archive and a DeleteMany.
// Every case here is a way it could wrongly pass, so each one asserts a
// failure. The only test that expects success is the first.

func TestVerifyByReadingAcceptsAFaithfulCopy(t *testing.T) {
	body := "documents, compressed"
	d := &fakeDestination{content: body}

	err := VerifyByReading(context.Background(), d, StoredObject{
		Key: "archive.ndjson.gz", Size: int64(len(body)), SHA256: sum(body),
	})
	if err != nil {
		t.Fatalf("a faithful copy failed verification: %v", err)
	}
}

func TestVerifyByReadingRejectsCorruption(t *testing.T) {
	body := "documents, compressed"

	cases := []struct {
		name    string
		stored  string
		obj     StoredObject
		wantErr string
	}{
		{
			// The case metadata comparison cannot see: right length, wrong
			// bytes. This is what silent corruption and a wrong-key overwrite
			// both look like from the outside.
			name:    "same length, different bytes",
			stored:  strings.Repeat("x", len(body)),
			obj:     StoredObject{Size: int64(len(body)), SHA256: sum(body)},
			wantErr: "hashes to",
		},
		{
			name:    "truncated",
			stored:  body[:5],
			obj:     StoredObject{Size: int64(len(body)), SHA256: sum(body)},
			wantErr: "bytes, expected",
		},
		{
			name:    "longer than expected",
			stored:  body + " and then some",
			obj:     StoredObject{Size: int64(len(body)), SHA256: sum(body)},
			wantErr: "bytes, expected",
		},
		{
			// "Nothing to compare against" must never read as "matches". An
			// empty expected digest is a bug upstream, and failing closed is
			// the only safe response to it.
			name:    "no expected checksum",
			stored:  body,
			obj:     StoredObject{Size: int64(len(body)), SHA256: ""},
			wantErr: "no expected checksum",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.obj.Key = "archive.ndjson.gz"
			err := VerifyByReading(context.Background(), &fakeDestination{content: tc.stored}, tc.obj)
			if err == nil {
				t.Fatal("verification passed a copy that is not the archive")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error does not explain the failure: %v", err)
			}
		})
	}
}

// A destination that cannot be read has not been verified. Reporting the
// difference matters: an unreachable bucket is an outage, not corruption, but
// both must block deletion.
func TestVerifyByReadingRejectsAnUnreadableCopy(t *testing.T) {
	sentinel := errors.New("bucket is on fire")
	d := &fakeDestination{openErr: sentinel}

	err := VerifyByReading(context.Background(), d, StoredObject{
		Key: "archive.ndjson.gz", Size: 21, SHA256: sum("documents, compressed"),
	})
	if err == nil {
		t.Fatal("verification passed a copy it could not read")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the underlying cause was lost: %v", err)
	}
}

// A read that dies partway through is not a short object — it is an unknown,
// and unknown has to fail.
func TestVerifyByReadingRejectsATruncatedRead(t *testing.T) {
	d := &fakeDestination{content: "documents, compressed", readErr: errors.New("connection reset")}

	err := VerifyByReading(context.Background(), d, StoredObject{
		Key: "archive.ndjson.gz", Size: 21, SHA256: sum("documents, compressed"),
	})
	if err == nil {
		t.Fatal("verification passed a copy whose download failed midway")
	}
}

func TestArchivalStorageClassesAreNotVerifiable(t *testing.T) {
	archival := []string{"GLACIER", "DEEP_ARCHIVE", "glacier", " Glacier_IR ", "ARCHIVE"}
	for _, class := range archival {
		if !isArchivalClass(class) {
			t.Errorf("%q should be treated as archival: an object there cannot be read back without a restore", class)
		}
	}

	readable := []string{"", "STANDARD", "STANDARD_IA", "ONEZONE_IA", "INTELLIGENT_TIERING"}
	for _, class := range readable {
		if isArchivalClass(class) {
			t.Errorf("%q is readable and should not block verification", class)
		}
	}
}

// --- a destination that stores one object ----------------------------------

type fakeDestination struct {
	content string
	openErr error
	readErr error
}

func (f *fakeDestination) Alias() string               { return "fake" }
func (f *fakeDestination) Info() Info                  { return Info{Alias: "fake"} }
func (f *fakeDestination) Check(context.Context) error { return nil }
func (f *fakeDestination) Key(key string) string       { return key }
func (f *fakeDestination) Verify(context.Context, StoredObject) error {
	return errors.New("not used by these tests")
}

func (f *fakeDestination) Upload(_ context.Context, _ string, body io.Reader, _ int64, _ Metadata) (string, error) {
	raw, err := io.ReadAll(body)
	f.content = string(raw)
	return "handle", err
}

func (f *fakeDestination) Open(context.Context, string, string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.readErr != nil {
		return io.NopCloser(io.MultiReader(
			strings.NewReader(f.content[:len(f.content)/2]),
			errReader{f.readErr},
		)), nil
	}
	return io.NopCloser(bytes.NewReader([]byte(f.content))), nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
