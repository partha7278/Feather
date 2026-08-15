// Package bundle moves a whole feather installation as one file: the MongoDB
// accounts, the backup destinations, the retention configs, and the instance
// settings, exported together and importable into another feather.
//
// Credentials are the reason this is not just a JSON dump. They are why feather
// has a master key at all, and a bundle by definition leaves the machine that
// holds that key — so on the way out every sealed value is re-sealed under a
// passphrase the operator types, with scrypt standing between that passphrase
// and the AES key. A bundle can also be written with no credentials in it,
// which is the form to keep in version control or hand to somebody else.
package bundle

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/secrets"
	"github.com/unifers/feather/internal/store"
)

// Version is the bundle format. An older feather refuses a newer bundle rather
// than importing the half of it that it happens to understand.
const Version = 1

// MinPassphraseLength matches the floor on an administrator password: the
// passphrase is the only thing between a stolen bundle and every bucket and
// cluster it names.
const MinPassphraseLength = 12

var (
	// ErrPassphraseRequired means the bundle carries credentials and none was given.
	ErrPassphraseRequired = errors.New("this bundle carries credentials — the passphrase used to export it is required")
	// ErrWrongPassphrase is a passphrase that does not open the bundle.
	ErrWrongPassphrase = errors.New("that passphrase does not open this bundle")
	// ErrNotABundle is a file that is not a feather export at all.
	ErrNotABundle = errors.New("this file is not a feather bundle")
)

// Bundle is the whole exported installation.
type Bundle struct {
	Feather      Header              `json:"feather"`
	Settings     Settings            `json:"settings"`
	MongoSources []MongoSource       `json:"mongoSources"`
	Destinations []Destination       `json:"destinations"`
	Configs      []*config.JobConfig `json:"configs"`
}

// Header says what wrote this file and how to open what is in it.
type Header struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`
	Instance   string    `json:"instance,omitempty"`
	Host       string    `json:"host,omitempty"`

	// Secrets is absent when the bundle was exported without credentials.
	Secrets *Envelope `json:"secrets,omitempty"`
}

// Envelope is how the credentials in this bundle were sealed. The parameters
// travel with the file so a future feather can still open an old export after
// the defaults have been raised.
type Envelope struct {
	KDF   string `json:"kdf"`   // "scrypt"
	N     int    `json:"n"`     // cost
	R     int    `json:"r"`     // block size
	P     int    `json:"p"`     // parallelism
	Salt  string `json:"salt"`  // base64
	Check string `json:"check"` // a known plaintext, sealed — so a wrong passphrase is named as such
}

// Settings are the process-wide values the control panel can edit.
type Settings struct {
	InstanceName      string `json:"instanceName,omitempty"`
	MaxConcurrentJobs int    `json:"maxConcurrentJobs,omitempty"`
}

// MongoSource is one exported MongoDB account. URI is sealed; URIPreview is the
// redacted form, carried so the file says something to a person reading it.
type MongoSource struct {
	Alias      string `json:"alias"`
	URIPreview string `json:"uriPreview,omitempty"`
	URI        string `json:"uri,omitempty"`
	AuthSource string `json:"authSource,omitempty"`
	AppName    string `json:"appName,omitempty"`
	Enabled    bool   `json:"enabled"`
	Note       string `json:"note,omitempty"`
}

// Destination is one exported backup account. The five sealed fields are
// exactly the five the database itself encrypts.
type Destination struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Prefix   string `json:"prefix,omitempty"`
	Enabled  bool   `json:"enabled"`
	Note     string `json:"note,omitempty"`

	Region         string `json:"region,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	AccessKeyID    string `json:"accessKeyId,omitempty"`
	StorageClass   string `json:"storageClass,omitempty"`
	ForcePathStyle bool   `json:"forcePathStyle,omitempty"`

	DriveFolderID string `json:"driveFolderId,omitempty"`
	DriveID       string `json:"driveId,omitempty"`
	DriveSubject  string `json:"driveSubject,omitempty"`
	OAuthClientID string `json:"oauthClientId,omitempty"`

	SecretAccessKey    string `json:"secretAccessKey,omitempty"`
	SessionToken       string `json:"sessionToken,omitempty"`
	ServiceAccountJSON string `json:"serviceAccountJson,omitempty"`
	OAuthClientSecret  string `json:"oauthClientSecret,omitempty"`
	OAuthRefreshToken  string `json:"oauthRefreshToken,omitempty"`
}

// ExportOptions decides whether the credentials come along.
type ExportOptions struct {
	Passphrase     string
	WithoutSecrets bool
}

// ImportOptions decides what an import is allowed to touch.
type ImportOptions struct {
	Passphrase string
	// Overwrite replaces entries whose alias or name already exists. Off, an
	// import only adds what is missing, which is what makes it safe to repeat.
	Overwrite bool
	// Settings applies the instance name and concurrency cap from the bundle.
	Settings bool
}

// --- export ----------------------------------------------------------------

// Export reads the whole installation out of the database. Nothing is written:
// the caller decides where the bundle goes.
func Export(ctx context.Context, a *app.App, opts ExportOptions) (*Bundle, error) {
	sealer, envelope, err := newSealer(opts)
	if err != nil {
		return nil, err
	}

	instance, err := a.Store.Meta(ctx, app.MetaInstanceName, "")
	if err != nil {
		return nil, err
	}

	out := &Bundle{
		Feather: Header{
			Version:    Version,
			ExportedAt: time.Now().UTC(),
			Instance:   instance,
			Host:       a.Host,
			Secrets:    envelope,
		},
		Settings: Settings{
			InstanceName:      instance,
			MaxConcurrentJobs: a.MaxConcurrentJobs(ctx),
		},
	}

	sources, err := a.Store.ListMongoSources(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range sources {
		entry := MongoSource{
			Alias:      record.Alias,
			URIPreview: record.Preview,
			AuthSource: record.AuthSource,
			AppName:    record.AppName,
			Enabled:    record.Enabled,
			Note:       record.Note,
		}
		if entry.URI, err = sealer.seal(record.URI); err != nil {
			return nil, fmt.Errorf("MongoDB account %q: %w", record.Alias, err)
		}
		out.MongoSources = append(out.MongoSources, entry)
	}

	destinations, err := a.Store.ListDestinations(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range destinations {
		entry := Destination{
			Alias:          record.Alias,
			Provider:       string(record.Provider),
			Prefix:         record.Prefix,
			Enabled:        record.Enabled,
			Note:           record.Note,
			Region:         record.Region,
			Bucket:         record.Bucket,
			Endpoint:       record.Endpoint,
			AccessKeyID:    record.AccessKeyID,
			StorageClass:   record.StorageClass,
			ForcePathStyle: record.ForcePathStyle,
			DriveFolderID:  record.DriveFolderID,
			DriveID:        record.DriveID,
			DriveSubject:   record.DriveSubject,
			OAuthClientID:  record.OAuthClientID,
		}
		for _, credential := range []struct {
			into *string
			from string
		}{
			{&entry.SecretAccessKey, record.SecretAccessKey},
			{&entry.SessionToken, record.SessionToken},
			{&entry.ServiceAccountJSON, record.ServiceAccountJSON},
			{&entry.OAuthClientSecret, record.OAuthClientSecret},
			{&entry.OAuthRefreshToken, record.OAuthRefreshToken},
		} {
			if *credential.into, err = sealer.seal(credential.from); err != nil {
				return nil, fmt.Errorf("destination %q: %w", record.Alias, err)
			}
		}
		out.Destinations = append(out.Destinations, entry)
	}

	jobs, err := a.Store.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	out.Configs = jobs

	return out, nil
}

// Encode renders the bundle as indented JSON — a bundle is a file an operator
// will open and read, and the sealed fields are unreadable enough already.
func (b *Bundle) Encode() ([]byte, error) {
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// FileName is what a download should be called.
func (b *Bundle) FileName() string {
	name := b.Feather.Instance
	if name == "" {
		name = b.Feather.Host
	}
	if name == "" {
		name = "feather"
	}
	stamp := b.Feather.ExportedAt.Format("20060102-1504")
	return fmt.Sprintf("feather-%s-%s.json", safeFileSegment(name), stamp)
}

// HasSecrets reports whether this bundle carries credentials.
func (b *Bundle) HasSecrets() bool { return b.Feather.Secrets != nil }

// --- import ----------------------------------------------------------------

// Decode parses a bundle file and checks it is one, without needing the
// passphrase — so a caller can ask for the passphrase only when it is needed.
func Decode(data []byte) (*Bundle, error) {
	var out Bundle
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotABundle, err)
	}
	if out.Feather.Version == 0 {
		return nil, ErrNotABundle
	}
	if out.Feather.Version > Version {
		return nil, fmt.Errorf("this bundle is format version %d; this build of feather understands version %d — upgrade feather first",
			out.Feather.Version, Version)
	}
	return &out, nil
}

// Apply writes the bundle into the database. Entries that already exist are
// left alone unless Overwrite is set, so importing the same bundle twice is
// harmless. Anything that cannot be applied is reported rather than aborting
// the rest: a merge into a running instance is expected to have leftovers.
//
// The caller reloads the process afterwards — until then the changes are stored
// but not live.
func (b *Bundle) Apply(ctx context.Context, a *app.App, opts ImportOptions) (*Result, error) {
	opener, err := b.opener(opts.Passphrase)
	if err != nil {
		return nil, err
	}
	result := &Result{}

	existingSources, err := a.Store.ListMongoSources(ctx)
	if err != nil {
		return nil, err
	}
	sourceByAlias := make(map[string]store.MongoSourceRecord, len(existingSources))
	for _, record := range existingSources {
		sourceByAlias[record.Alias] = record
	}

	for _, entry := range b.MongoSources {
		existing, exists := sourceByAlias[entry.Alias]
		if exists && !opts.Overwrite {
			result.MongoSources.Skipped++
			result.note("MongoDB account %q already exists", entry.Alias)
			continue
		}
		// The passphrase was checked against the envelope before any of this, so
		// a value that will not open here is a damaged file rather than a wrong
		// passphrase — one entry's problem, not the whole import's.
		uri, err := opener.open(entry.URI)
		if err != nil {
			result.MongoSources.Skipped++
			result.note("MongoDB account %q: its connection string is damaged in this bundle", entry.Alias)
			continue
		}
		// A bundle exported without credentials can refresh an account that is
		// already here, but it cannot conjure one that is not.
		if !exists && uri == "" {
			result.MongoSources.Skipped++
			result.note("MongoDB account %q has no connection string in this bundle — add it by hand", entry.Alias)
			continue
		}

		record := store.MongoSourceRecord{
			ID:         existing.ID,
			Alias:      entry.Alias,
			URI:        uri,
			AuthSource: entry.AuthSource,
			AppName:    entry.AppName,
			Enabled:    entry.Enabled,
			Note:       entry.Note,
		}
		if err := a.Store.SaveMongoSource(ctx, &record); err != nil {
			result.MongoSources.Skipped++
			result.note("MongoDB account %q: %v", entry.Alias, err)
			continue
		}
		result.MongoSources.count(exists)
	}

	existingDestinations, err := a.Store.ListDestinations(ctx)
	if err != nil {
		return nil, err
	}
	destinationByAlias := make(map[string]store.DestinationRecord, len(existingDestinations))
	for _, record := range existingDestinations {
		destinationByAlias[record.Alias] = record
	}

	for _, entry := range b.Destinations {
		existing, exists := destinationByAlias[entry.Alias]
		if exists && !opts.Overwrite {
			result.Destinations.Skipped++
			result.note("destination %q already exists", entry.Alias)
			continue
		}

		record := store.DestinationRecord{
			ID:      existing.ID,
			Enabled: entry.Enabled,
			Note:    entry.Note,
			Destination: config.Destination{
				Alias:          entry.Alias,
				Provider:       config.Provider(entry.Provider),
				Prefix:         entry.Prefix,
				Region:         entry.Region,
				Bucket:         entry.Bucket,
				Endpoint:       entry.Endpoint,
				AccessKeyID:    entry.AccessKeyID,
				StorageClass:   entry.StorageClass,
				ForcePathStyle: entry.ForcePathStyle,
				DriveFolderID:  entry.DriveFolderID,
				DriveID:        entry.DriveID,
				DriveSubject:   entry.DriveSubject,
				OAuthClientID:  entry.OAuthClientID,
			},
		}
		damaged := false
		for _, credential := range []struct {
			into *string
			from string
		}{
			{&record.SecretAccessKey, entry.SecretAccessKey},
			{&record.SessionToken, entry.SessionToken},
			{&record.ServiceAccountJSON, entry.ServiceAccountJSON},
			{&record.OAuthClientSecret, entry.OAuthClientSecret},
			{&record.OAuthRefreshToken, entry.OAuthRefreshToken},
		} {
			plain, err := opener.open(credential.from)
			if err != nil {
				damaged = true
				break
			}
			*credential.into = plain
		}
		if damaged {
			result.Destinations.Skipped++
			result.note("destination %q: its credentials are damaged in this bundle", entry.Alias)
			continue
		}

		if err := a.Store.SaveDestination(ctx, &record); err != nil {
			result.Destinations.Skipped++
			result.note("destination %q: %v", entry.Alias, err)
			continue
		}
		result.Destinations.count(exists)
	}

	existingJobs, err := a.Store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	jobByName := make(map[string]store.JobRecord, len(existingJobs))
	for _, record := range existingJobs {
		jobByName[record.Config.Name] = record
	}

	for _, job := range b.Configs {
		if job == nil {
			continue
		}
		existing, exists := jobByName[job.Name]
		if exists && !opts.Overwrite {
			result.Configs.Skipped++
			result.note("retention config %q already exists", job.Name)
			continue
		}
		record := store.JobRecord{ID: existing.ID, Config: job}
		if err := a.Store.SaveJob(ctx, &record); err != nil {
			result.Configs.Skipped++
			result.note("retention config %q: %v", job.Name, err)
			continue
		}
		result.Configs.count(exists)
	}

	// Settings are only reported as applied when they actually move, so a repeat
	// import reads as the no-op it is.
	if opts.Settings {
		if b.Settings.InstanceName != "" {
			current, err := a.Store.Meta(ctx, app.MetaInstanceName, "")
			if err != nil {
				return nil, err
			}
			if current != b.Settings.InstanceName {
				if err := a.Store.SetMeta(ctx, app.MetaInstanceName, b.Settings.InstanceName); err != nil {
					return nil, err
				}
				result.Settings = true
			}
		}
		if b.Settings.MaxConcurrentJobs > 0 {
			current, err := a.Store.MetaInt(ctx, app.MetaMaxConcurrentJobs, 0)
			if err != nil {
				return nil, err
			}
			if current != b.Settings.MaxConcurrentJobs {
				if err := a.Store.SetMeta(ctx, app.MetaMaxConcurrentJobs,
					strconv.Itoa(b.Settings.MaxConcurrentJobs)); err != nil {
					return nil, err
				}
				result.Settings = true
			}
		}
	}

	return result, nil
}

// Result is what an import actually did.
type Result struct {
	MongoSources Counts
	Destinations Counts
	Configs      Counts
	Settings     bool
	Notes        []string
}

// Counts is the outcome for one kind of entry.
type Counts struct {
	Created int
	Updated int
	Skipped int
}

func (c *Counts) count(existed bool) {
	if existed {
		c.Updated++
		return
	}
	c.Created++
}

func (c Counts) String() string {
	var parts []string
	if c.Created > 0 {
		parts = append(parts, fmt.Sprintf("%d added", c.Created))
	}
	if c.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", c.Updated))
	}
	if c.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", c.Skipped))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// Applied is how many entries the import created or changed.
func (r *Result) Applied() int {
	total := 0
	for _, c := range []Counts{r.MongoSources, r.Destinations, r.Configs} {
		total += c.Created + c.Updated
	}
	return total
}

// Summary is one sentence for a flash message or a terminal.
func (r *Result) Summary() string {
	line := fmt.Sprintf("MongoDB accounts: %s. Destinations: %s. Retention configs: %s.",
		r.MongoSources, r.Destinations, r.Configs)
	if r.Settings {
		line += " Instance settings applied."
	}
	return line
}

func (r *Result) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// --- sealing ---------------------------------------------------------------

// scrypt parameters for a new export: ~32MB and a fraction of a second, which
// is a real cost to an attacker grinding passphrases and no cost to an operator
// who types one twice a year.
const (
	scryptN = 1 << 15
	scryptR = 8
	scryptP = 1

	// checkPlaintext is sealed into the envelope so a wrong passphrase can be
	// named as such instead of surfacing as a decryption failure on whichever
	// account happened to be first.
	checkPlaintext = "feather-bundle"
)

// sealer seals and opens the credentials in one bundle. A nil box means the
// bundle carries none.
type sealer struct{ box *secrets.Box }

func newSealer(opts ExportOptions) (sealer, *Envelope, error) {
	if opts.WithoutSecrets {
		return sealer{}, nil, nil
	}
	if len([]rune(opts.Passphrase)) < MinPassphraseLength {
		return sealer{}, nil, fmt.Errorf("the export passphrase must be at least %d characters", MinPassphraseLength)
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return sealer{}, nil, err
	}
	box, err := deriveBox(opts.Passphrase, salt, scryptN, scryptR, scryptP)
	if err != nil {
		return sealer{}, nil, err
	}

	s := sealer{box: box}
	check, err := s.seal(checkPlaintext)
	if err != nil {
		return sealer{}, nil, err
	}
	return s, &Envelope{
		KDF:   "scrypt",
		N:     scryptN,
		R:     scryptR,
		P:     scryptP,
		Salt:  base64.StdEncoding.EncodeToString(salt),
		Check: check,
	}, nil
}

// opener builds the sealer that reads this bundle, and rejects a wrong
// passphrase before a single row is written.
func (b *Bundle) opener(passphrase string) (sealer, error) {
	envelope := b.Feather.Secrets
	if envelope == nil {
		return sealer{}, nil
	}
	if passphrase == "" {
		return sealer{}, ErrPassphraseRequired
	}
	if envelope.KDF != "scrypt" {
		return sealer{}, fmt.Errorf("this bundle uses an unknown key derivation %q", envelope.KDF)
	}
	salt, err := base64.StdEncoding.DecodeString(envelope.Salt)
	if err != nil || len(salt) == 0 {
		return sealer{}, fmt.Errorf("%w: the salt is unreadable", ErrNotABundle)
	}
	// The parameters come out of a file somebody else wrote; an absurd cost
	// would otherwise be a way to make feather allocate gigabytes on demand.
	if err := checkScryptParams(envelope.N, envelope.R, envelope.P); err != nil {
		return sealer{}, err
	}

	box, err := deriveBox(passphrase, salt, envelope.N, envelope.R, envelope.P)
	if err != nil {
		return sealer{}, err
	}
	s := sealer{box: box}

	plain, err := s.open(envelope.Check)
	if err != nil || subtle.ConstantTimeCompare([]byte(plain), []byte(checkPlaintext)) != 1 {
		return sealer{}, ErrWrongPassphrase
	}
	return s, nil
}

func deriveBox(passphrase string, salt []byte, n, r, p int) (*secrets.Box, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, n, r, p, secrets.KeySize)
	if err != nil {
		return nil, fmt.Errorf("deriving the bundle key: %w", err)
	}
	return secrets.New(key, "export passphrase")
}

func checkScryptParams(n, r, p int) error {
	if n < 1<<10 || n > 1<<20 || n&(n-1) != 0 {
		return fmt.Errorf("this bundle asks for an scrypt cost of %d, which feather will not use", n)
	}
	if r < 1 || r > 16 || p < 1 || p > 4 {
		return fmt.Errorf("this bundle asks for scrypt parameters feather will not use (r=%d, p=%d)", r, p)
	}
	return nil
}

func (s sealer) seal(plain string) (string, error) {
	if s.box == nil || plain == "" {
		return "", nil
	}
	sealed, err := s.box.Seal(plain)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s sealer) open(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if s.box == nil {
		return "", ErrPassphraseRequired
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: a sealed value is not valid base64", ErrNotABundle)
	}
	plain, err := s.box.Open(sealed)
	if err != nil {
		return "", ErrWrongPassphrase
	}
	return plain, nil
}

// safeFileSegment keeps an instance name usable as part of a download name.
func safeFileSegment(raw string) string {
	var b strings.Builder
	separated := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if separated && b.Len() > 0 {
				b.WriteByte('-')
			}
			separated = false
			b.WriteRune(r)
		default:
			separated = true
		}
	}
	return b.String()
}
