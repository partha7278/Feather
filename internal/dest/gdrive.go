package dest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/unifers/feather/internal/config"
)

// driveFolderMIME is what Drive calls a folder. Drive has no paths, so the
// folders a key template implies are real objects feather has to create.
const driveFolderMIME = "application/vnd.google-apps.folder"

// driveUploadChunk is how much of an archive the API client buffers per
// request. 16 MiB matches the S3 part size and keeps a resumable upload's
// footprint predictable on a small VPS.
const driveUploadChunk = 16 << 20

// GDriveDestination writes archives into a Google Drive folder. The object key
// a run generates is treated as a path: every segment but the last becomes a
// folder under the configured root, and the last becomes the file.
type GDriveDestination struct {
	info    Info
	svc     *drive.Service
	root    string // folder ID archives are filed under, or "root"
	driveID string // shared drive the root lives in, if any

	// Drive resolves a path by walking it, so the walk is cached. The mutex
	// also serialises folder creation, which is what stops two batches from
	// racing and creating the same folder twice.
	mu      sync.Mutex
	folders map[string]string
}

// NewGDriveDestination builds a Drive client for one configured destination.
func NewGDriveDestination(ctx context.Context, cfg config.Destination) (*GDriveDestination, error) {
	// The client holds on to this context for token refreshes, which outlive
	// the request that built it — a reload from the UI would otherwise leave a
	// destination whose credentials expire and never renew.
	ctx = context.WithoutCancel(ctx)

	opts, err := driveAuthOptions(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("destination %s: %w", cfg.Alias, err)
	}
	svc, err := drive.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("destination %s: building the Google Drive client: %w", cfg.Alias, err)
	}

	return &GDriveDestination{
		info: Info{
			Alias:    cfg.Alias,
			Provider: string(config.ProviderGDrive),
			Bucket:   cfg.DriveRoot(),
			Prefix:   cfg.Prefix,
		},
		svc:     svc,
		root:    cfg.DriveRoot(),
		driveID: cfg.DriveID,
		folders: map[string]string{},
	}, nil
}

// driveAuthOptions turns stored credentials into client options. A service
// account key is used as-is unless a subject is set, which is domain-wide
// delegation; otherwise an OAuth client refreshes the stored token itself.
func driveAuthOptions(ctx context.Context, cfg config.Destination) ([]option.ClientOption, error) {
	switch {
	case cfg.ServiceAccountJSON != "":
		if cfg.DriveSubject != "" {
			jwtCfg, err := google.JWTConfigFromJSON([]byte(cfg.ServiceAccountJSON), drive.DriveScope)
			if err != nil {
				return nil, fmt.Errorf("reading the service account key: %w", err)
			}
			jwtCfg.Subject = cfg.DriveSubject
			return []option.ClientOption{option.WithTokenSource(jwtCfg.TokenSource(ctx))}, nil
		}
		return []option.ClientOption{
			option.WithCredentialsJSON([]byte(cfg.ServiceAccountJSON)),
			option.WithScopes(drive.DriveScope),
		}, nil

	case cfg.OAuthRefreshToken != "":
		oauthCfg := &oauth2.Config{
			ClientID:     cfg.OAuthClientID,
			ClientSecret: cfg.OAuthClientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       []string{drive.DriveScope},
		}
		token := &oauth2.Token{RefreshToken: cfg.OAuthRefreshToken}
		return []option.ClientOption{option.WithTokenSource(oauthCfg.TokenSource(ctx, token))}, nil

	default:
		return nil, errors.New("no Google Drive credentials: add a service account key, or connect an OAuth client")
	}
}

func (d *GDriveDestination) Alias() string { return d.info.Alias }
func (d *GDriveDestination) Info() Info    { return d.info }

// Check confirms the credentials work and that the destination folder exists
// and accepts new children, which is the Drive equivalent of a HeadBucket.
func (d *GDriveDestination) Check(ctx context.Context) error {
	folder, err := d.svc.Files.Get(d.root).
		Fields("id,name,mimeType,capabilities/canAddChildren,driveId,trashed").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("destination %s: cannot open Drive folder %s: %w", d.info.Alias, d.root, driveError(err))
	}
	if folder.MimeType != driveFolderMIME {
		return fmt.Errorf("destination %s: %s is a file (%s), not a folder", d.info.Alias, d.root, folder.MimeType)
	}
	if folder.Trashed {
		return fmt.Errorf("destination %s: Drive folder %q is in the trash", d.info.Alias, folder.Name)
	}
	if folder.Capabilities != nil && !folder.Capabilities.CanAddChildren {
		return fmt.Errorf("destination %s: this account can see Drive folder %q but cannot add files to it — share it with edit access", d.info.Alias, folder.Name)
	}
	return nil
}

// Key applies the destination's own prefix, exactly as the S3 client does. The
// result is still a slash-separated path; Drive folders are made from it at
// upload time.
func (d *GDriveDestination) Key(key string) string {
	if d.info.Prefix == "" {
		return key
	}
	return strings.TrimSuffix(d.info.Prefix, "/") + "/" + key
}

// Upload creates the folders the key implies and writes the archive into the
// last of them. Re-uploading the same key replaces the file's content rather
// than adding a second file with the same name, which Drive would happily do.
func (d *GDriveDestination) Upload(ctx context.Context, key string, body io.Reader, size int64, meta Metadata) (string, error) {
	dir, name := path.Split(key)
	if name == "" {
		return "", fmt.Errorf("uploading to %s: object key %q does not end in a file name", d.info.Alias, key)
	}

	parent, err := d.ensureFolders(ctx, dir)
	if err != nil {
		return "", err
	}

	file := &drive.File{
		Name: name,
		AppProperties: map[string]string{
			metaSHA256:   meta.SHA256,
			metaDocCount: strconv.FormatInt(meta.DocCount, 10),
			metaConfig:   meta.Config,
			metaRunID:    meta.RunID,
			metaColl:     meta.Collection,
		},
	}
	mediaOpts := []googleapi.MediaOption{
		googleapi.ContentType(contentTypeFor(key)),
		googleapi.ChunkSize(driveUploadChunk),
	}

	existing, err := d.findChild(ctx, parent, name)
	if err != nil {
		return "", err
	}

	var stored *drive.File
	if existing == "" {
		file.Parents = []string{parent}
		stored, err = d.svc.Files.Create(file).
			Media(body, mediaOpts...).
			Fields("id,size").
			SupportsAllDrives(true).
			Context(ctx).Do()
	} else {
		// Parents cannot be set on an update; the file is already in the right
		// folder, since that is how it was found.
		stored, err = d.svc.Files.Update(existing, file).
			Media(body, mediaOpts...).
			Fields("id,size").
			SupportsAllDrives(true).
			Context(ctx).Do()
	}
	if err != nil {
		return "", fmt.Errorf("uploading to %s (%s): %w", d.info.Alias, key, driveError(err))
	}
	// Drive has no ETag worth keeping; the file ID is what an operator needs to
	// find this copy again, so the catalogue's etag column holds that instead.
	return stored.Id, nil
}

// Verify re-reads the stored file's metadata and compares its size and
// checksum. Drive computes a SHA-256 of its own, which is a stronger check than
// the one feather records alongside the file, so both are compared when present.
func (d *GDriveDestination) Verify(ctx context.Context, key string, wantSize int64, wantSHA256 string) error {
	dir, name := path.Split(key)

	parent, err := d.resolveFolders(ctx, dir)
	if err != nil {
		return fmt.Errorf("verifying %s (%s): %w", d.info.Alias, key, err)
	}
	id, err := d.findChild(ctx, parent, name)
	if err != nil {
		return fmt.Errorf("verifying %s (%s): %w", d.info.Alias, key, err)
	}
	if id == "" {
		return fmt.Errorf("verifying %s (%s): the uploaded file is not in the folder it was written to", d.info.Alias, key)
	}

	file, err := d.svc.Files.Get(id).
		Fields("id,size,sha256Checksum,appProperties,trashed").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("verifying %s (%s): %w", d.info.Alias, key, driveError(err))
	}
	if file.Trashed {
		return fmt.Errorf("verifying %s (%s): the stored file is in the trash", d.info.Alias, key)
	}
	if file.Size != wantSize {
		return fmt.Errorf("verifying %s (%s): stored file is %d bytes, expected %d", d.info.Alias, key, file.Size, wantSize)
	}
	if file.Sha256Checksum != "" && !strings.EqualFold(file.Sha256Checksum, wantSHA256) {
		return fmt.Errorf("verifying %s (%s): Drive's checksum %s does not match %s", d.info.Alias, key, file.Sha256Checksum, wantSHA256)
	}
	if recorded := file.AppProperties[metaSHA256]; recorded != "" && !strings.EqualFold(recorded, wantSHA256) {
		return fmt.Errorf("verifying %s (%s): stored checksum %s does not match %s", d.info.Alias, key, recorded, wantSHA256)
	}
	return nil
}

// ensureFolders walks a slash-separated path from the destination's root,
// creating folders that are not there yet, and returns the last folder's ID.
func (d *GDriveDestination) ensureFolders(ctx context.Context, dir string) (string, error) {
	return d.walk(ctx, dir, true)
}

// resolveFolders walks the same path without creating anything, which is what
// verification wants: a missing folder there means the upload did not land.
func (d *GDriveDestination) resolveFolders(ctx context.Context, dir string) (string, error) {
	return d.walk(ctx, dir, false)
}

func (d *GDriveDestination) walk(ctx context.Context, dir string, create bool) (string, error) {
	// One walk at a time per destination. Batches upload sequentially, so the
	// contention is between concurrent jobs sharing a destination — rare, and
	// far cheaper than two runs creating duplicate folders for the same path.
	d.mu.Lock()
	defer d.mu.Unlock()

	parent := d.root
	walked := ""
	for _, segment := range strings.Split(strings.Trim(dir, "/"), "/") {
		if segment == "" || segment == "." {
			continue
		}
		walked = path.Join(walked, segment)
		if cached, ok := d.folders[walked]; ok {
			parent = cached
			continue
		}

		id, err := d.findChild(ctx, parent, segment)
		if err != nil {
			return "", err
		}
		if id == "" {
			if !create {
				return "", fmt.Errorf("Drive folder %q does not exist under %s", walked, d.root)
			}
			created, err := d.svc.Files.Create(&drive.File{
				Name:     segment,
				MimeType: driveFolderMIME,
				Parents:  []string{parent},
			}).Fields("id").SupportsAllDrives(true).Context(ctx).Do()
			if err != nil {
				return "", fmt.Errorf("creating Drive folder %q in %s: %w", walked, d.info.Alias, driveError(err))
			}
			id = created.Id
		}

		d.folders[walked] = id
		parent = id
	}
	return parent, nil
}

// findChild looks for one named child of a folder, returning "" when there is
// none. Drive allows several files to share a name in one folder; the newest
// wins, since that is the one an overwriting upload most recently wrote.
func (d *GDriveDestination) findChild(ctx context.Context, parent, name string) (string, error) {
	query := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false", driveQuote(name), driveQuote(parent))

	call := d.svc.Files.List().
		Q(query).
		Fields("files(id,name)").
		OrderBy("createdTime desc").
		PageSize(1).
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true)
	if d.driveID != "" {
		call = call.Corpora("drive").DriveId(d.driveID)
	}

	list, err := call.Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("looking for %q in %s: %w", name, d.info.Alias, driveError(err))
	}
	if len(list.Files) == 0 {
		return "", nil
	}
	return list.Files[0].Id, nil
}

// driveQuote escapes a value for a Drive search query, where strings are
// single-quoted and backslash is the escape character.
func driveQuote(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `'`, `\'`)
}

// driveError unwraps the API client's error envelope, and names the two
// failures that are otherwise baffling: a service account with no storage of
// its own, and a Drive API that was never switched on for the project.
func driveError(err error) error {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return err
	}

	message := apiErr.Message
	if message == "" {
		message = apiErr.Body
	}
	switch {
	case strings.Contains(message, "storageQuotaExceeded"), strings.Contains(message, "Service Accounts do not have storage quota"):
		return fmt.Errorf("%s — a service account has no Drive storage of its own; write into a shared drive, or use an OAuth client instead", message)
	case apiErr.Code == 403 && strings.Contains(message, "has not been used in project"):
		return fmt.Errorf("%s — enable the Google Drive API for this project", message)
	case apiErr.Code == 404:
		return fmt.Errorf("not found: %s (check the folder ID, and that it is shared with this account)", message)
	}
	return fmt.Errorf("%d: %s", apiErr.Code, message)
}
