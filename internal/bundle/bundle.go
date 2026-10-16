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
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
	ErrNotABundle = errors.New("this file is not a Feather bundle")
	// ErrTampered is a sealed bundle whose contents no longer match what was
	// exported: some field beside the credentials has been edited.
	ErrTampered = errors.New("this bundle has been changed since it was exported — nothing was imported")
	// ErrUnauthenticated is a sealed bundle written before Feather authenticated
	// the whole file. Its credentials cannot be trusted to belong to the
	// endpoints written beside them, so they are not imported.
	ErrUnauthenticated = errors.New("this bundle was exported by an older Feather that did not protect its contents " +
		"against editing — export it again from the source installation to import its credentials")
)

// Bundle is the whole exported installation.
type Bundle struct {
	Feather      Header        `json:"feather"`
	Settings     Settings      `json:"settings"`
	MongoSources []MongoSource `json:"mongoSources"`
	// PostgresSources is omitted when empty, so a bundle from an installation
	// with none reads exactly as one written before PostgreSQL existed.
	PostgresSources []PostgresSource    `json:"postgresSources,omitempty"`
	Destinations    []Destination       `json:"destinations"`
	Configs         []*config.JobConfig `json:"configs"`

	// Runs is the execution log: finished runs, the files they wrote and
	// where those files went. Only present when it was chosen.
	Runs []store.RunHistory `json:"runs,omitempty"`
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

	// MAC authenticates everything else in the bundle under a key derived from
	// the same passphrase, so an endpoint, bucket or config edited after export
	// is refused instead of being imported beside the genuine credentials.
	MAC string `json:"mac,omitempty"` // base64 HMAC-SHA256
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

// PostgresSource is one exported PostgreSQL account. DSN is sealed;
// DSNPreview is the redacted form.
type PostgresSource struct {
	Alias      string `json:"alias"`
	DSNPreview string `json:"dsnPreview,omitempty"`
	DSN        string `json:"dsn,omitempty"`
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

// Sections picks which parts of an installation an export carries or an import
// applies.
type Sections struct {
	MongoSources    bool
	PostgresSources bool
	Destinations    bool
	Configs         bool
	Settings        bool
	Runs            bool
}

// AllSections is every part of an installation, execution logs included.
func AllSections() Sections {
	return Sections{MongoSources: true, PostgresSources: true, Destinations: true, Configs: true, Settings: true, Runs: true}
}

// defaultSections is what a bundle carried before the parts could be chosen.
// The execution log is left out: it can be large, and an export that grows by
// itself is not what a scripted caller expects.
func defaultSections() Sections {
	all := AllSections()
	all.Runs = false
	return all
}

// None reports whether nothing at all is picked.
func (s Sections) None() bool { return s == Sections{} }

// carriesCredentials reports whether any picked part holds sealed values.
func (s Sections) carriesCredentials() bool {
	return s.MongoSources || s.PostgresSources || s.Destinations
}

// ExportOptions decides what comes along, and whether the credentials do.
type ExportOptions struct {
	Passphrase     string
	WithoutSecrets bool
	// Sections picks what is exported. Left zero, everything is.
	Sections Sections
}

func (o ExportOptions) sections() Sections {
	if o.Sections.None() {
		return defaultSections()
	}
	return o.Sections
}

// ImportOptions decides what an import is allowed to touch.
type ImportOptions struct {
	Passphrase string
	// Overwrite replaces entries whose alias or name already exists. Off, an
	// import only adds what is missing, which is what makes it safe to repeat.
	Overwrite bool
	// Settings applies the instance name and concurrency cap from the bundle.
	// It is only consulted when Sections is left zero.
	Settings bool
	// Sections picks what is applied. Left zero, every account, destination and
	// config is, and the settings follow Settings.
	Sections Sections
}

func (o ImportOptions) sections() Sections {
	if o.Sections.None() {
		all := defaultSections()
		all.Settings = o.Settings
		return all
	}
	return o.Sections
}

// --- export ----------------------------------------------------------------

// Export reads the whole installation out of the database. Nothing is written:
// the caller decides where the bundle goes.
func Export(ctx context.Context, a *app.App, opts ExportOptions) (*Bundle, error) {
	sections := opts.sections()
	// Configs and settings hold nothing secret, so a bundle of only those needs
	// no passphrase.
	if !sections.carriesCredentials() {
		opts.WithoutSecrets = true
	}
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
	}
	if sections.Settings {
		out.Settings = Settings{
			InstanceName:      instance,
			MaxConcurrentJobs: a.MaxConcurrentJobs(ctx),
		}
	}

	if sections.MongoSources {
		if err := exportMongoSources(ctx, a, sealer, out); err != nil {
			return nil, err
		}
	}
	if sections.PostgresSources {
		if err := exportPostgresSources(ctx, a, sealer, out); err != nil {
			return nil, err
		}
	}
	if sections.Destinations {
		if err := exportDestinations(ctx, a, sealer, out); err != nil {
			return nil, err
		}
	}
	if sections.Configs {
		jobs, err := a.Store.Jobs(ctx)
		if err != nil {
			return nil, err
		}
		out.Configs = jobs
	}
	if sections.Runs {
		runs, err := a.Store.FinishedRunHistory(ctx)
		if err != nil {
			return nil, err
		}
		out.Runs = runs
	}

	if envelope != nil {
		mac, err := sealer.authenticate(out)
		if err != nil {
			return nil, err
		}
		envelope.MAC = base64.StdEncoding.EncodeToString(mac)
	}
	return out, nil
}

func exportMongoSources(ctx context.Context, a *app.App, sealer sealer, out *Bundle) error {
	sources, err := a.Store.ListMongoSources(ctx)
	if err != nil {
		return err
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
		if entry.URI, err = sealer.seal(record.URI, sealContext("mongoSources", record.Alias, "uri")); err != nil {
			return fmt.Errorf("MongoDB account %q: %w", record.Alias, err)
		}
		out.MongoSources = append(out.MongoSources, entry)
	}
	return nil
}

func exportPostgresSources(ctx context.Context, a *app.App, sealer sealer, out *Bundle) error {
	sources, err := a.Store.ListPostgresSources(ctx)
	if err != nil {
		return err
	}
	for _, record := range sources {
		entry := PostgresSource{
			Alias:      record.Alias,
			DSNPreview: record.Preview,
			AppName:    record.AppName,
			Enabled:    record.Enabled,
			Note:       record.Note,
		}
		if entry.DSN, err = sealer.seal(record.DSN, sealContext("postgresSources", record.Alias, "dsn")); err != nil {
			return fmt.Errorf("PostgreSQL account %q: %w", record.Alias, err)
		}
		out.PostgresSources = append(out.PostgresSources, entry)
	}
	return nil
}

func exportDestinations(ctx context.Context, a *app.App, sealer sealer, out *Bundle) error {
	destinations, err := a.Store.ListDestinations(ctx)
	if err != nil {
		return err
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
			field string
			into  *string
			from  string
		}{
			{"secretAccessKey", &entry.SecretAccessKey, record.SecretAccessKey},
			{"sessionToken", &entry.SessionToken, record.SessionToken},
			{"serviceAccountJson", &entry.ServiceAccountJSON, record.ServiceAccountJSON},
			{"oauthClientSecret", &entry.OAuthClientSecret, record.OAuthClientSecret},
			{"oauthRefreshToken", &entry.OAuthRefreshToken, record.OAuthRefreshToken},
		} {
			where := sealContext("destinations", record.Alias, credential.field)
			if *credential.into, err = sealer.seal(credential.from, where); err != nil {
				return fmt.Errorf("destination %q: %w", record.Alias, err)
			}
		}
		out.Destinations = append(out.Destinations, entry)
	}
	return nil
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
	if out.Feather.Version < 1 {
		return nil, ErrNotABundle
	}
	if out.Feather.Version > Version {
		return nil, fmt.Errorf("this bundle is format version %d; this build of Feather understands version %d — upgrade Feather first",
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
	sections := opts.sections()
	// Only accounts and destinations are sealed, so an import of configs or
	// settings alone does not ask for the passphrase. Given one anyway, a sealed
	// bundle that can be authenticated is, before anything is written.
	var opener sealer
	if sections.carriesCredentials() || (opts.Passphrase != "" && b.HasSecrets() && b.Feather.Secrets.MAC != "") {
		var err error
		if opener, err = b.opener(opts.Passphrase); err != nil {
			return nil, err
		}
	}
	result := &Result{}

	if sections.MongoSources {
		if err := b.applyMongoSources(ctx, a, opener, opts.Overwrite, result); err != nil {
			return nil, err
		}
	}
	if sections.PostgresSources {
		if err := b.applyPostgresSources(ctx, a, opener, opts.Overwrite, result); err != nil {
			return nil, err
		}
	}
	if sections.Destinations {
		if err := b.applyDestinations(ctx, a, opener, opts.Overwrite, result); err != nil {
			return nil, err
		}
	}
	if sections.Configs {
		if err := b.applyConfigs(ctx, a, opts.Overwrite, result); err != nil {
			return nil, err
		}
	}
	if sections.Settings {
		if err := b.applySettings(ctx, a, result); err != nil {
			return nil, err
		}
	}
	if sections.Runs {
		b.applyRuns(ctx, a, result)
	}
	return result, nil
}

// applyRuns adds the runs this installation has not seen. A run is history, so
// Overwrite does not apply: one already here is the same run, and is skipped.
func (b *Bundle) applyRuns(ctx context.Context, a *app.App, result *Result) {
	for _, run := range b.Runs {
		added, err := a.Store.ImportRunHistory(ctx, run)
		switch {
		case err != nil:
			result.Runs.Skipped++
			result.note("run %s: %v", run.Run.ID, err)
		case added:
			result.Runs.Created++
		default:
			result.Runs.Skipped++
		}
	}
}

func (b *Bundle) applyMongoSources(ctx context.Context, a *app.App, opener sealer, overwrite bool, result *Result) error {
	existingSources, err := a.Store.ListMongoSources(ctx)
	if err != nil {
		return err
	}
	sourceByAlias := make(map[string]store.MongoSourceRecord, len(existingSources))
	for _, record := range existingSources {
		sourceByAlias[record.Alias] = record
	}

	for _, entry := range b.MongoSources {
		existing, exists := sourceByAlias[entry.Alias]
		if exists && !overwrite {
			result.MongoSources.Skipped++
			result.note("MongoDB account %q already exists", entry.Alias)
			continue
		}
		// The passphrase was checked against the envelope before any of this, so
		// a value that will not open here is a damaged file rather than a wrong
		// passphrase — one entry's problem, not the whole import's.
		uri, err := opener.open(entry.URI, sealContext("mongoSources", entry.Alias, "uri"))
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
	return nil
}

// applyPostgresSources follows applyMongoSources rule for rule: add what is
// missing, replace only with Overwrite, and skip — with a note — an entry that
// cannot be opened or has no connection string to create from.
func (b *Bundle) applyPostgresSources(ctx context.Context, a *app.App, opener sealer, overwrite bool, result *Result) error {
	existingSources, err := a.Store.ListPostgresSources(ctx)
	if err != nil {
		return err
	}
	byAlias := make(map[string]store.PostgresSourceRecord, len(existingSources))
	for _, record := range existingSources {
		byAlias[record.Alias] = record
	}

	for _, entry := range b.PostgresSources {
		existing, exists := byAlias[entry.Alias]
		if exists && !overwrite {
			result.PostgresSources.Skipped++
			result.note("PostgreSQL account %q already exists", entry.Alias)
			continue
		}
		dsn, err := opener.open(entry.DSN, sealContext("postgresSources", entry.Alias, "dsn"))
		if err != nil {
			result.PostgresSources.Skipped++
			result.note("PostgreSQL account %q: its connection string is damaged in this bundle", entry.Alias)
			continue
		}
		if !exists && dsn == "" {
			result.PostgresSources.Skipped++
			result.note("PostgreSQL account %q has no connection string in this bundle — add it by hand", entry.Alias)
			continue
		}

		record := store.PostgresSourceRecord{
			ID:      existing.ID,
			Alias:   entry.Alias,
			DSN:     dsn,
			AppName: entry.AppName,
			Enabled: entry.Enabled,
			Note:    entry.Note,
		}
		if err := a.Store.SavePostgresSource(ctx, &record); err != nil {
			result.PostgresSources.Skipped++
			result.note("PostgreSQL account %q: %v", entry.Alias, err)
			continue
		}
		result.PostgresSources.count(exists)
	}
	return nil
}

func (b *Bundle) applyDestinations(ctx context.Context, a *app.App, opener sealer, overwrite bool, result *Result) error {
	existingDestinations, err := a.Store.ListDestinations(ctx)
	if err != nil {
		return err
	}
	destinationByAlias := make(map[string]store.DestinationRecord, len(existingDestinations))
	for _, record := range existingDestinations {
		destinationByAlias[record.Alias] = record
	}

	for _, entry := range b.Destinations {
		existing, exists := destinationByAlias[entry.Alias]
		if exists && !overwrite {
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
			field string
			into  *string
			from  string
		}{
			{"secretAccessKey", &record.SecretAccessKey, entry.SecretAccessKey},
			{"sessionToken", &record.SessionToken, entry.SessionToken},
			{"serviceAccountJson", &record.ServiceAccountJSON, entry.ServiceAccountJSON},
			{"oauthClientSecret", &record.OAuthClientSecret, entry.OAuthClientSecret},
			{"oauthRefreshToken", &record.OAuthRefreshToken, entry.OAuthRefreshToken},
		} {
			plain, err := opener.open(credential.from, sealContext("destinations", entry.Alias, credential.field))
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
		// An entry without credentials may refresh an account, but not move it:
		// the stored secret would follow it to wherever the file now points.
		if exists && hasCredentials(existing.Destination) && !hasCredentials(record.Destination) &&
			retargets(existing.Destination, record.Destination) {
			result.Destinations.Skipped++
			result.note("destination %q points somewhere else in this bundle but carries no credentials for it — "+
				"import a bundle with credentials or edit it by hand", entry.Alias)
			continue
		}

		if err := a.Store.SaveDestination(ctx, &record); err != nil {
			result.Destinations.Skipped++
			result.note("destination %q: %v", entry.Alias, err)
			continue
		}
		result.Destinations.count(exists)
	}
	return nil
}

// hasCredentials reports whether a destination carries any secret at all.
func hasCredentials(d config.Destination) bool {
	return d.SecretAccessKey != "" || d.SessionToken != "" || d.ServiceAccountJSON != "" ||
		d.OAuthClientSecret != "" || d.OAuthRefreshToken != ""
}

// retargets reports whether incoming names a different place, or a different
// identity, than the stored account its credentials were entered for.
func retargets(stored, incoming config.Destination) bool {
	return stored.Provider != incoming.Provider ||
		stored.Endpoint != incoming.Endpoint ||
		stored.Region != incoming.Region ||
		stored.Bucket != incoming.Bucket ||
		stored.AccessKeyID != incoming.AccessKeyID ||
		stored.DriveFolderID != incoming.DriveFolderID ||
		stored.DriveID != incoming.DriveID ||
		stored.DriveSubject != incoming.DriveSubject ||
		stored.OAuthClientID != incoming.OAuthClientID
}

func (b *Bundle) applyConfigs(ctx context.Context, a *app.App, overwrite bool, result *Result) error {
	existingJobs, err := a.Store.ListJobs(ctx)
	if err != nil {
		return err
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
		if exists && !overwrite {
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
	return nil
}

// applySettings only reports the settings as applied when they actually move,
// so a repeat import reads as the no-op it is.
func (b *Bundle) applySettings(ctx context.Context, a *app.App, result *Result) error {
	if b.Settings.InstanceName != "" {
		current, err := a.Store.Meta(ctx, app.MetaInstanceName, "")
		if err != nil {
			return err
		}
		if current != b.Settings.InstanceName {
			if err := a.Store.SetMeta(ctx, app.MetaInstanceName, b.Settings.InstanceName); err != nil {
				return err
			}
			result.Settings = true
		}
	}
	if b.Settings.MaxConcurrentJobs > 0 {
		current, err := a.Store.MetaInt(ctx, app.MetaMaxConcurrentJobs, 0)
		if err != nil {
			return err
		}
		if current != b.Settings.MaxConcurrentJobs {
			if err := a.Store.SetMeta(ctx, app.MetaMaxConcurrentJobs,
				strconv.Itoa(b.Settings.MaxConcurrentJobs)); err != nil {
				return err
			}
			result.Settings = true
		}
	}
	return nil
}

// Result is what an import actually did.
type Result struct {
	MongoSources    Counts
	PostgresSources Counts
	Destinations    Counts
	Configs         Counts
	Runs            Counts
	Settings        bool
	Notes           []string
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
	for _, c := range []Counts{r.MongoSources, r.PostgresSources, r.Destinations, r.Configs, r.Runs} {
		total += c.Created + c.Updated
	}
	return total
}

// Summary is one sentence for a flash message or a terminal.
func (r *Result) Summary() string {
	line := fmt.Sprintf("MongoDB accounts: %s.", r.MongoSources)
	if r.PostgresSources != (Counts{}) {
		line += fmt.Sprintf(" PostgreSQL accounts: %s.", r.PostgresSources)
	}
	line += fmt.Sprintf(" Destinations: %s. Retention configs: %s.", r.Destinations, r.Configs)
	if r.Runs != (Counts{}) {
		line += fmt.Sprintf(" Execution logs: %s.", r.Runs)
	}
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

// sealer seals and opens the credentials in one bundle, and authenticates the
// rest of it. A nil aead means the bundle carries no credentials.
type sealer struct {
	aead   cipher.AEAD
	macKey []byte
}

// sealContext names where a sealed value belongs. It is the AEAD's additional
// data, so a value moved to another field or another entry does not open.
func sealContext(section, alias, field string) string {
	encoded, _ := json.Marshal([]string{section, alias, field})
	return string(encoded)
}

// checkContext is the additional data the envelope's check value is sealed with.
var checkContext = sealContext("feather", "", "check")

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
	s, err := deriveSealer(opts.Passphrase, salt, scryptN, scryptR, scryptP)
	if err != nil {
		return sealer{}, nil, err
	}

	check, err := s.seal(checkPlaintext, checkContext)
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

// opener builds the sealer that reads this bundle, rejects a wrong passphrase,
// and refuses a bundle whose contents have changed — all before a single row is
// written.
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
	// Without a MAC nothing ties the sealed values to the endpoints beside
	// them, and stripping it must not be a way around the check below.
	if envelope.MAC == "" {
		return sealer{}, ErrUnauthenticated
	}
	mac, err := base64.StdEncoding.DecodeString(envelope.MAC)
	if err != nil {
		return sealer{}, ErrTampered
	}

	s, err := deriveSealer(passphrase, salt, envelope.N, envelope.R, envelope.P)
	if err != nil {
		return sealer{}, err
	}

	plain, err := s.open(envelope.Check, checkContext)
	if err != nil || subtle.ConstantTimeCompare([]byte(plain), []byte(checkPlaintext)) != 1 {
		return sealer{}, ErrWrongPassphrase
	}
	want, err := s.authenticate(b)
	if err != nil {
		return sealer{}, err
	}
	if !hmac.Equal(mac, want) {
		return sealer{}, ErrTampered
	}
	return s, nil
}

// deriveSealer stretches the passphrase with scrypt, then splits the result
// into independent keys for sealing and for authenticating.
func deriveSealer(passphrase string, salt []byte, n, r, p int) (sealer, error) {
	master, err := scrypt.Key([]byte(passphrase), salt, n, r, p, secrets.KeySize)
	if err != nil {
		return sealer{}, fmt.Errorf("deriving the bundle key: %w", err)
	}
	sealKey, err := hkdf.Key(sha256.New, master, nil, "feather bundle: seal credentials", secrets.KeySize)
	if err != nil {
		return sealer{}, err
	}
	macKey, err := hkdf.Key(sha256.New, master, nil, "feather bundle: authenticate contents", sha256.Size)
	if err != nil {
		return sealer{}, err
	}
	block, err := aes.NewCipher(sealKey)
	if err != nil {
		return sealer{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealer{}, err
	}
	return sealer{aead: aead, macKey: macKey}, nil
}

// checkScryptParams bounds what a bundle may ask scrypt for. Memory is
// 128·N·r bytes, and p repeats that work, so the bounds are on the products,
// not on each parameter alone.
func checkScryptParams(n, r, p int) error {
	if n < 1<<10 || n > 1<<20 || n&(n-1) != 0 {
		return fmt.Errorf("this bundle asks for an scrypt cost of %d, which Feather will not use", n)
	}
	if r < 1 || r > 16 || p < 1 || p > 4 {
		return fmt.Errorf("this bundle asks for scrypt parameters Feather will not use (r=%d, p=%d)", r, p)
	}
	const maxMemory = 256 << 20 // eight times what a new export asks for
	if memory := 128 * int64(n) * int64(r); memory > maxMemory {
		return fmt.Errorf("this bundle asks for scrypt parameters needing %d MiB, which Feather will not use", memory>>20)
	}
	if work := 128 * int64(n) * int64(r) * int64(p); work > 4*maxMemory {
		return fmt.Errorf("this bundle asks for scrypt parameters Feather will not use (N=%d, r=%d, p=%d)", n, r, p)
	}
	return nil
}

func (s sealer) seal(plain, where string) (string, error) {
	if s.aead == nil || plain == "" {
		return "", nil
	}
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plain), []byte(where))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s sealer) open(encoded, where string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if s.aead == nil {
		return "", ErrPassphraseRequired
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: a sealed value is not valid base64", ErrNotABundle)
	}
	if len(sealed) < s.aead.NonceSize() {
		return "", ErrWrongPassphrase
	}
	nonce, body := sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, body, []byte(where))
	if err != nil {
		return "", ErrWrongPassphrase
	}
	return string(plain), nil
}

// authenticate is the MAC over everything in the bundle but the MAC itself.
//
// Each sealed value is already authenticated, and bound to its place, by the
// AEAD; here it stands as a fixed marker of its presence. That keeps one
// damaged value one entry's problem — it is skipped on import — while removing
// or adding a credential still fails the whole file.
//
// The encoding is the bundle's own JSON after one round trip through Decode,
// so the bytes hashed at export are the bytes a reader rebuilds from the file.
func (s sealer) authenticate(b *Bundle) ([]byte, error) {
	encoded, err := json.Marshal(b.canonical())
	if err != nil {
		return nil, err
	}
	var normalized Bundle
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	if encoded, err = json.Marshal(normalized.canonical()); err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.macKey)
	mac.Write(encoded)
	return mac.Sum(nil), nil
}

// canonical is a copy of the bundle with the MAC removed and each sealed value
// replaced by a marker. The original is left untouched.
func (b *Bundle) canonical() *Bundle {
	const present = "sealed"
	marker := func(v string) string {
		if v == "" {
			return ""
		}
		return present
	}

	out := *b
	if b.Feather.Secrets != nil {
		envelope := *b.Feather.Secrets
		envelope.MAC = ""
		envelope.Check = marker(envelope.Check)
		out.Feather.Secrets = &envelope
	}
	if b.MongoSources != nil {
		out.MongoSources = make([]MongoSource, len(b.MongoSources))
		for i, entry := range b.MongoSources {
			entry.URI = marker(entry.URI)
			out.MongoSources[i] = entry
		}
	}
	if b.PostgresSources != nil {
		out.PostgresSources = make([]PostgresSource, len(b.PostgresSources))
		for i, entry := range b.PostgresSources {
			entry.DSN = marker(entry.DSN)
			out.PostgresSources[i] = entry
		}
	}
	if b.Destinations != nil {
		out.Destinations = make([]Destination, len(b.Destinations))
		for i, entry := range b.Destinations {
			entry.SecretAccessKey = marker(entry.SecretAccessKey)
			entry.SessionToken = marker(entry.SessionToken)
			entry.ServiceAccountJSON = marker(entry.ServiceAccountJSON)
			entry.OAuthClientSecret = marker(entry.OAuthClientSecret)
			entry.OAuthRefreshToken = marker(entry.OAuthRefreshToken)
			out.Destinations[i] = entry
		}
	}
	return &out
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
