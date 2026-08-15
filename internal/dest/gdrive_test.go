package dest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/unifers/feather/internal/config"
)

// The Drive path is the one destination feather cannot exercise against a local
// rig the way MinIO covers S3, so these tests run it against a small stand-in
// for the API: enough of files.list, files.create and files.get to prove that a
// key becomes folders, that an archive lands in the last of them, and that
// verification compares what was actually stored.

func TestGDriveUploadCreatesTheFoldersAKeyImplies(t *testing.T) {
	drive, destination := newFakeDrive(t)
	ctx := context.Background()

	const key = "feather/shop/orders/2026-05/orders-00001.ndjson.gz"
	body := "documents, compressed"

	id, err := destination.Upload(ctx, key, strings.NewReader(body), int64(len(body)), Metadata{
		SHA256: sum(body), DocCount: 12, Config: "orders-archive", RunID: "run-1", Collection: "shop.orders",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("no Drive file ID came back for the catalogue")
	}

	stored, ok := drive.files[id]
	if !ok {
		t.Fatalf("file %s is not in Drive", id)
	}
	if stored.name != "orders-00001.ndjson.gz" {
		t.Errorf("the file is called %q", stored.name)
	}
	if stored.content != body {
		t.Errorf("Drive holds %q", stored.content)
	}
	if got := drive.pathOf(id); got != key {
		t.Errorf("the file was filed at %q, want %q", got, key)
	}
	// The metadata that makes verification meaningful travels with the file.
	if stored.properties[metaSHA256] != sum(body) || stored.properties[metaRunID] != "run-1" {
		t.Errorf("app properties are %v", stored.properties)
	}

	// A second batch into the same month must reuse those folders rather than
	// create a second set beside them.
	before := drive.folderCount()
	if _, err := destination.Upload(ctx, "feather/shop/orders/2026-05/orders-00002.ndjson.gz",
		strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)}); err != nil {
		t.Fatal(err)
	}
	if after := drive.folderCount(); after != before {
		t.Errorf("a second upload created %d more folders", after-before)
	}
}

func TestGDriveVerifyChecksSizeAndChecksum(t *testing.T) {
	_, destination := newFakeDrive(t)
	ctx := context.Background()

	const key = "feather/orders-00001.ndjson.gz"
	body := "documents, compressed"
	if _, err := destination.Upload(ctx, key, strings.NewReader(body), int64(len(body)),
		Metadata{SHA256: sum(body)}); err != nil {
		t.Fatal(err)
	}

	good := StoredObject{Key: key, Size: int64(len(body)), SHA256: sum(body)}
	if err := destination.Verify(ctx, good); err != nil {
		t.Fatalf("a good copy failed verification: %v", err)
	}

	wrongSize := good
	wrongSize.Size++
	if err := destination.Verify(ctx, wrongSize); err == nil {
		t.Error("verification passed a file of the wrong size")
	}

	wrongSum := good
	wrongSum.SHA256 = sum("something else")
	if err := destination.Verify(ctx, wrongSum); err == nil {
		t.Error("verification passed a file with the wrong checksum")
	}

	// An expected digest of "" must not be treated as "nothing to compare, so
	// it matches" — that would wave through every copy on the deletion path.
	noSum := good
	noSum.SHA256 = ""
	if err := destination.Verify(ctx, noSum); err == nil {
		t.Error("verification passed a copy it had no checksum to compare")
	}

	// A missing file is the failure that must never be mistaken for success:
	// deletion from MongoDB is what happens on the other side of this check.
	missing := StoredObject{Key: "feather/never-written.ndjson.gz", Size: 1, SHA256: sum(body)}
	if err := destination.Verify(ctx, missing); err == nil {
		t.Error("verification passed a file that was never uploaded")
	}
}

// The point of the rewrite: verification reads the stored bytes rather than
// trusting metadata. A file whose content was replaced underneath feather, but
// whose size and recorded checksum still say what they always said, has to
// fail — because that is what silent corruption looks like from the outside.
func TestGDriveVerifyReadsTheStoredBytes(t *testing.T) {
	drive, destination := newFakeDrive(t)
	ctx := context.Background()

	const key = "feather/orders-00001.ndjson.gz"
	body := "documents, compressed"

	id, err := destination.Upload(ctx, key, strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)})
	if err != nil {
		t.Fatal(err)
	}

	obj := StoredObject{Key: key, Handle: id, Size: int64(len(body)), SHA256: sum(body)}
	if err := destination.Verify(ctx, obj); err != nil {
		t.Fatalf("a good copy failed verification: %v", err)
	}

	// Same length, different bytes. Nothing about the size tells you.
	drive.files[id].content = strings.Repeat("x", len(body))
	if err := destination.Verify(ctx, obj); err == nil {
		t.Fatal("verification passed a copy whose bytes had been replaced")
	}
}

// Drive's query index lags writes, so a verification that finds the file by
// name can miss one that is certainly there. The handle Upload returned is used
// instead, and has to work even when a name search would find nothing.
func TestGDriveVerifyUsesTheHandleNotTheName(t *testing.T) {
	drive, destination := newFakeDrive(t)
	ctx := context.Background()

	const key = "feather/orders-00001.ndjson.gz"
	body := "documents, compressed"

	id, err := destination.Upload(ctx, key, strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)})
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for the lagging index: the file is there, but not findable by
	// name. Verification by handle must be unaffected.
	drive.files[id].name = "not-what-the-search-looks-for"

	obj := StoredObject{Key: key, Handle: id, Size: int64(len(body)), SHA256: sum(body)}
	if err := destination.Verify(ctx, obj); err != nil {
		t.Fatalf("verification by handle failed: %v", err)
	}

	// Without the handle, the same copy is unfindable — which is exactly the
	// spurious failure the handle exists to avoid.
	obj.Handle = ""
	if err := destination.Verify(ctx, obj); err == nil {
		t.Error("a name search found a file whose name had changed")
	}
}

// Re-uploading a key replaces the file rather than leaving two files with the
// same name in one folder, which Drive would otherwise allow.
func TestGDriveReuploadReplacesTheFile(t *testing.T) {
	drive, destination := newFakeDrive(t)
	ctx := context.Background()

	const key = "feather/orders-00001.ndjson.gz"
	first, err := destination.Upload(ctx, key, strings.NewReader("one"), 3, Metadata{SHA256: sum("one")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := destination.Upload(ctx, key, strings.NewReader("two"), 3, Metadata{SHA256: sum("two")})
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Errorf("a second upload created a new file (%s then %s)", first, second)
	}
	if drive.files[second].content != "two" {
		t.Errorf("the file holds %q", drive.files[second].content)
	}
	if err := destination.Verify(ctx, StoredObject{Key: key, Handle: second, Size: 3, SHA256: sum("two")}); err != nil {
		t.Fatalf("the replaced file does not verify: %v", err)
	}
}

func TestGDriveCheckRejectsAFolderItCannotWriteTo(t *testing.T) {
	drive, destination := newFakeDrive(t)
	ctx := context.Background()

	if err := destination.Check(ctx); err != nil {
		t.Fatalf("a writable folder was rejected: %v", err)
	}

	drive.files[destination.root].readOnly = true
	err := destination.Check(ctx)
	if err == nil {
		t.Fatal("a folder feather cannot add files to was accepted")
	}
	if !strings.Contains(err.Error(), "edit access") {
		t.Errorf("the error does not say what to do about it: %v", err)
	}
}

func TestGDriveKeyAppliesThePrefix(t *testing.T) {
	destination := &GDriveDestination{info: Info{Prefix: "cold/"}}
	if got := destination.Key("shop/orders.ndjson.gz"); got != "cold/shop/orders.ndjson.gz" {
		t.Errorf("prefixed key is %q", got)
	}

	bare := &GDriveDestination{}
	if got := bare.Key("shop/orders.ndjson.gz"); got != "shop/orders.ndjson.gz" {
		t.Errorf("unprefixed key is %q", got)
	}
}

// Names go into a query language that quotes with ' and escapes with \.
func TestDriveQuoteEscapesQueryStrings(t *testing.T) {
	cases := map[string]string{
		"orders.ndjson.gz":  "orders.ndjson.gz",
		"o'brien":           `o\'brien`,
		`back\slash`:        `back\\slash`,
		`' or trashed = ' `: `\' or trashed = \' `,
	}
	for input, want := range cases {
		if got := driveQuote(input); got != want {
			t.Errorf("%q quoted to %q, want %q", input, got, want)
		}
	}
}

// --- a stand-in for the Drive API ------------------------------------------

type fakeFile struct {
	id, name, mimeType, parent, content string
	properties                          map[string]string
	readOnly                            bool
}

type fakeDrive struct {
	files map[string]*fakeFile
	next  int
}

const fakeRootID = "root-folder"

// newFakeDrive starts the stand-in and returns a destination pointed at it.
func newFakeDrive(t *testing.T) (*fakeDrive, *GDriveDestination) {
	t.Helper()

	fake := &fakeDrive{files: map[string]*fakeFile{
		fakeRootID: {id: fakeRootID, name: "archives", mimeType: driveFolderMIME},
	}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)

	svc, err := drive.NewService(context.Background(),
		option.WithEndpoint(server.URL+"/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}

	return fake, &GDriveDestination{
		info:    Info{Alias: "drive", Provider: string(config.ProviderGDrive), Bucket: fakeRootID},
		svc:     svc,
		root:    fakeRootID,
		folders: map[string]string{},
	}
}

func (f *fakeDrive) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3")
	path = strings.TrimPrefix(path, "/")

	switch {
	case r.Method == http.MethodGet && path == "files":
		f.list(w, r)
	case r.Method == http.MethodPost && path == "files":
		f.write(w, r, "")
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "files/"):
		f.write(w, r, strings.TrimPrefix(path, "files/"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "files/"):
		f.get(w, r, strings.TrimPrefix(path, "files/"))
	default:
		http.Error(w, `{"error":{"code":404,"message":"no such method"}}`, http.StatusNotFound)
	}
}

var (
	queryName   = regexp.MustCompile(`name = '((?:[^'\\]|\\.)*)'`)
	queryParent = regexp.MustCompile(`'((?:[^'\\]|\\.)*)' in parents`)
)

func (f *fakeDrive) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	name := unquote(queryName.FindStringSubmatch(query))
	parent := unquote(queryParent.FindStringSubmatch(query))

	out := &drive.FileList{}
	for _, file := range f.files {
		if file.name == name && file.parent == parent {
			out.Files = append(out.Files, &drive.File{Id: file.id, Name: file.name})
		}
	}
	writeJSON(w, out)
}

// write covers both files.create and files.update, with or without media.
func (f *fakeDrive) write(w http.ResponseWriter, r *http.Request, id string) {
	metadata, media, err := decodeUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, ok := f.files[id]
	if !ok {
		f.next++
		file = &fakeFile{id: fmt.Sprintf("file-%d", f.next)}
		if len(metadata.Parents) > 0 {
			file.parent = metadata.Parents[0]
		}
		f.files[file.id] = file
	}
	file.name = metadata.Name
	file.mimeType = metadata.MimeType
	file.properties = metadata.AppProperties
	file.content = media

	writeJSON(w, &drive.File{Id: file.id, Name: file.name, Size: int64(len(file.content))})
}

func (f *fakeDrive) get(w http.ResponseWriter, r *http.Request, id string) {
	file, ok := f.files[id]
	if !ok {
		http.Error(w, `{"error":{"code":404,"message":"File not found"}}`, http.StatusNotFound)
		return
	}

	// alt=media is the download: Drive returns the bytes, not the metadata.
	if r.URL.Query().Get("alt") == "media" {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, file.content)
		return
	}

	out := &drive.File{
		Id: file.id, Name: file.name, MimeType: file.mimeType,
		Size: int64(len(file.content)), Sha256Checksum: sum(file.content),
		AppProperties: file.properties,
		Capabilities:  &drive.FileCapabilities{CanAddChildren: !file.readOnly},
	}
	if file.mimeType != driveFolderMIME {
		// Drive reports no checksum for folders, and feather must not compare one.
		out.Capabilities = nil
	}
	writeJSON(w, out)
}

// pathOf rebuilds the slash-separated path of a file below the root, which is
// what the object key was supposed to become.
func (f *fakeDrive) pathOf(id string) string {
	segments := []string{}
	for file, ok := f.files[id]; ok && file.id != fakeRootID; file, ok = f.files[file.parent] {
		segments = append([]string{file.name}, segments...)
	}
	return strings.Join(segments, "/")
}

func (f *fakeDrive) folderCount() int {
	count := 0
	for _, file := range f.files {
		if file.mimeType == driveFolderMIME {
			count++
		}
	}
	return count
}

// decodeUpload reads either a bare metadata request or the multipart/related
// body the client sends when there is content too.
func decodeUpload(r *http.Request) (*drive.File, string, error) {
	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", err
	}

	metadata := &drive.File{}
	if !strings.HasPrefix(contentType, "multipart/") {
		return metadata, "", json.NewDecoder(r.Body).Decode(metadata)
	}

	var media strings.Builder
	parts := multipart.NewReader(r.Body, params["boundary"])
	for i := 0; ; i++ {
		part, err := parts.NextPart()
		if err == io.EOF {
			return metadata, media.String(), nil
		}
		if err != nil {
			return nil, "", err
		}
		if i == 0 {
			if err := json.NewDecoder(part).Decode(metadata); err != nil {
				return nil, "", err
			}
			continue
		}
		if _, err := io.Copy(&media, part); err != nil {
			return nil, "", err
		}
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func unquote(match []string) string {
	if len(match) < 2 {
		return ""
	}
	return strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(match[1])
}

func sum(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}
