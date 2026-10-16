package dest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Object keys are built from config names, database and collection names and
// timestamps. None of that may move an archive outside the destination's
// configured prefix or root folder, and none of it may change the meaning of
// the request it travels in.

// hostileKeys are run-generated keys an unusual collection or config name
// could produce.
var hostileKeys = []string{
	"../escape.ndjson.gz",
	"../../escape.ndjson.gz",
	"a/../../escape.ndjson.gz",
	"a/b/../../../escape.ndjson.gz",
	"./../escape.ndjson.gz",
	"/absolute/escape.ndjson.gz",
	"..",
}

// resolvesUnder reports whether key, once dot segments are resolved the way an
// HTTP server or path-normalising proxy resolves them, stays below prefix.
func resolvesUnder(prefix, key string) bool {
	cleaned := path.Clean("/" + key)
	want := "/" + strings.Trim(prefix, "/") + "/"
	return strings.HasPrefix(cleaned+"/", want) && cleaned+"/" != want
}

func TestS3KeyCannotClimbOutOfThePrefix(t *testing.T) {
	d := &S3Destination{info: Info{Prefix: "tenant-a"}}
	for _, key := range hostileKeys {
		if got := d.Key(key); !resolvesUnder("tenant-a", got) {
			t.Errorf("key %q became %q, which resolves outside tenant-a/", key, got)
		}
	}
}

// What the S3 client actually puts on the wire: a key is one path, escaped,
// with no way to add a query parameter or end the path early.
func TestS3KeysAreEscapedIntoASingleObjectPath(t *testing.T) {
	hermeticAWS(t)

	type request struct {
		method, escapedPath string
		query               url.Values
	}
	var (
		mu       sync.Mutex
		requests []request
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		requests = append(requests, request{r.Method, r.URL.EscapedPath(), r.URL.Query()})
		mu.Unlock()
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	cfg := s3Config("local", server.URL)
	cfg.Prefix = "tenant-a"
	d, err := NewS3Destination(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	cases := []string{
		"orders?acl=public-read.ndjson.gz",
		"orders#fragment.ndjson.gz",
		"orders%2F..%2F..%2Fescape.ndjson.gz",
		"orders&x-id=DeleteObject.ndjson.gz",
		"orders with spaces & 'quotes'.ndjson.gz",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			mu.Lock()
			requests = nil
			mu.Unlock()

			full := d.Key(key)
			body := "archive"
			if _, err := d.Upload(t.Context(), full, strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)}); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0].method != http.MethodPut {
				t.Fatalf("one upload became %+v", requests)
			}
			got := requests[0]
			decoded, err := url.PathUnescape(got.escapedPath)
			if err != nil {
				t.Fatal(err)
			}
			if want := "/archives/" + full; decoded != want {
				t.Errorf("the object path is %q, want %q", decoded, want)
			}
			// x-id=PutObject is the SDK's own marker; anything else came from the key.
			for name, values := range got.query {
				if name != "x-id" || len(values) != 1 || values[0] != "PutObject" {
					t.Errorf("the key smuggled a query parameter into the request: %v", got.query)
				}
			}
		})
	}
}

// The path the S3 request is sent to must not climb out of the prefix once a
// server or proxy resolves its dot segments.
func TestS3UploadPathStaysUnderThePrefixOnTheWire(t *testing.T) {
	hermeticAWS(t)

	var (
		mu    sync.Mutex
		paths []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	cfg := s3Config("local", server.URL)
	cfg.Prefix = "tenant-a"
	d, err := NewS3Destination(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"../escape.ndjson.gz", "a/../../escape.ndjson.gz"} {
		mu.Lock()
		paths = nil
		mu.Unlock()

		body := "archive"
		_, err := d.Upload(t.Context(), d.Key(key), strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)})
		if err != nil {
			// Refusing such a key is an acceptable answer.
			continue
		}
		mu.Lock()
		for _, p := range paths {
			decoded, _ := url.PathUnescape(p)
			if !resolvesUnder("archives/tenant-a", decoded) {
				t.Errorf("key %q was sent as %q, which resolves outside archives/tenant-a/", key, decoded)
			}
		}
		mu.Unlock()
	}
}

// Every file and folder Drive is asked to create for a prefixed destination
// must hang below the prefix folder, whatever the run-generated key says.
func TestGDriveUploadsStayBelowThePrefixFolder(t *testing.T) {
	keys := append(append([]string{}, hostileKeys...), "a/./b/../c.ndjson.gz", "//double//slash.ndjson.gz")
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			drive, destination := newFakeDrive(t)
			destination.info.Prefix = "tenant-a"
			body := "archive"
			id, err := destination.Upload(context.Background(), destination.Key(key),
				strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)})
			if err != nil {
				return // refusing is fine
			}
			prefixID := ""
			for fileID, file := range drive.files {
				if file.parent == fakeRootID && file.name == "tenant-a" {
					prefixID = fileID
				}
			}
			for fileID, file := range drive.files {
				if fileID == fakeRootID || fileID == prefixID {
					continue
				}
				if !drive.under(fileID, prefixID) {
					t.Errorf("%q was created outside the tenant-a folder, at %q", file.name, drive.pathOf(fileID))
				}
			}
			if _, ok := drive.files[id]; !ok {
				t.Errorf("upload returned %q, which Drive does not hold", id)
			}
		})
	}
}

// The folder cache is keyed by the joined path. A key with a ".." segment must
// not leave an entry that sends a later, ordinary key into the wrong folder —
// where a fresh process, walking the folders from scratch, will never find it.
func TestGDriveDotSegmentsDoNotMisfileALaterUpload(t *testing.T) {
	drive, destination := newFakeDrive(t)
	ctx := context.Background()
	body := "archive"

	if _, err := destination.Upload(ctx, "feather/../shop/first.ndjson.gz",
		strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)}); err != nil {
		// Refusing the dot segment is the other acceptable answer.
		t.Logf("a key with a .. segment was refused: %v", err)
	}

	id, err := destination.Upload(ctx, "shop/second.ndjson.gz",
		strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)})
	if err != nil {
		t.Fatal(err)
	}
	if got := drive.pathOf(id); got != "shop/second.ndjson.gz" {
		t.Errorf("shop/second.ndjson.gz was filed at %q", got)
	}

	// A restarted feather resolves the key by walking folders by name.
	fresh := &GDriveDestination{info: destination.info, svc: destination.svc, root: fakeRootID, folders: map[string]string{}}
	if _, err := fresh.Open(ctx, "shop/second.ndjson.gz", ""); err != nil {
		t.Errorf("after a restart the archive cannot be found by its key: %v", err)
	}
}

var driveQueryShape = regexp.MustCompile(
	`^name = '(?:[^'\\]|\\.)*' and '(?:[^'\\]|\\.)*' in parents and trashed = false$`)

// Folder and file names go into Drive's q= query language. A name carrying a
// quote must stay inside its string literal, so the query keeps its shape and
// cannot be widened to other files.
func TestGDriveQueriesKeepTheirShapeWithHostileNames(t *testing.T) {
	drive, destination := newFakeDrive(t)

	var (
		mu      sync.Mutex
		queries []string
	)
	inner := drive.serve
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("q"); q != "" {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
		}
		inner(w, r)
	}))
	t.Cleanup(server.Close)
	destination.svc.BasePath = server.URL + "/"

	names := []string{
		`o'brien`,
		`x' or name contains '`,
		`x\' or trashed = true or name = \'`,
		`back\slash\`,
		`' in parents or '`,
	}
	for _, name := range names {
		key := name + "/" + name + ".ndjson.gz"
		body := "archive"
		id, err := destination.Upload(context.Background(), key, strings.NewReader(body), int64(len(body)), Metadata{SHA256: sum(body)})
		if err != nil {
			t.Fatalf("uploading %q: %v", key, err)
		}
		if got := drive.pathOf(id); got != key {
			t.Errorf("%q was filed at %q", key, got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) == 0 {
		t.Fatal("no Drive queries were observed")
	}
	for _, q := range queries {
		if !driveQueryShape.MatchString(q) {
			t.Errorf("a query lost its shape: %s", q)
		}
	}
}

// under follows parents up from id and reports whether it passes through folder.
func (f *fakeDrive) under(id, folder string) bool {
	seen := map[string]bool{}
	for {
		file, ok := f.files[id]
		if !ok || seen[id] || folder == "" {
			return false
		}
		seen[id] = true
		if file.parent == folder {
			return true
		}
		id = file.parent
	}
}
