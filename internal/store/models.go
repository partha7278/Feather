package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/unifers/feather/internal/config"
)

// RunStatus is the lifecycle state of a single execution.
type RunStatus string

const (
	RunRunning RunStatus = "running"
	RunSuccess RunStatus = "success"
	RunPartial RunStatus = "partial"
	RunFailed  RunStatus = "failed"
	RunDryRun  RunStatus = "dry_run"
	RunSkipped RunStatus = "skipped"
)

// TriggerKind records what caused a run, so a surprising deletion can be traced
// back to the scheduler, the web UI, or a person at a terminal.
type TriggerKind string

const (
	TriggerCron   TriggerKind = "cron"
	TriggerManual TriggerKind = "manual"
	TriggerUI     TriggerKind = "ui"
)

// DestinationStatus tracks one copy of one artifact through upload and
// verification. Nothing is deleted from Mongo until every row reads "verified".
type DestinationStatus string

const (
	DestPending  DestinationStatus = "pending"
	DestUploaded DestinationStatus = "uploaded"
	DestVerified DestinationStatus = "verified"
	DestFailed   DestinationStatus = "failed"
)

type Run struct {
	ID              uuid.UUID
	ConfigName      string
	ConfigSHA256    string
	Database        string
	Collection      string
	FilterJSON      string
	Trigger         TriggerKind
	Host            string
	Status          RunStatus
	DryRun          bool
	StartedAt       time.Time
	FinishedAt      *time.Time
	DocsMatched     int64
	DocsScanned     int64
	DocsArchived    int64
	DocsDeleted     int64
	BytesRaw        int64
	BytesCompressed int64
	BatchCount      int
	Error           string
}

// Artifact is one archive file: what it is called, when it was written, how
// many documents it holds, how large it is, and whether the source documents
// were deleted afterwards.
type Artifact struct {
	ID                uuid.UUID
	RunID             uuid.UUID
	ConfigName        string
	Database          string
	Collection        string
	BatchSeq          int
	FileName          string
	Format            string
	Codec             string
	DocCount          int64
	BytesRaw          int64
	BytesCompressed   int64
	SHA256            string
	MinID             string
	MaxID             string
	MinTS             *time.Time
	MaxTS             *time.Time
	DeletedFromSource bool
	DeletedCount      int64
	DeletedAt         *time.Time
	CreatedAt         time.Time

	// Engine is the source engine the archive came from: config.EngineMongo
	// or config.EnginePostgres. It decides how the archive is restored.
	Engine string
}

// IsPostgres reports whether the archive holds PostgreSQL rows.
func (a Artifact) IsPostgres() bool { return a.Engine == config.EnginePostgres }

// ArtifactDestination is one stored copy of one artifact. LocationURL is the
// full path to that copy, which is what an operator actually needs when asked
// where some month's data went.
type ArtifactDestination struct {
	ID           uuid.UUID
	ArtifactID   uuid.UUID
	Alias        string
	Provider     string
	Bucket       string
	ObjectKey    string
	LocationURL  string
	Region       string
	Endpoint     string
	StorageClass string
	ETag         string
	SizeBytes    int64
	Status       DestinationStatus
	UploadedAt   *time.Time
	VerifiedAt   *time.Time
	Error        string
	CreatedAt    time.Time
}

// ArtifactWithCopies is one archive file together with every place it was
// stored — the shape the backup listing in the UI renders.
type ArtifactWithCopies struct {
	Artifact
	Copies []ArtifactDestination
}

// RunSummary is the compact view used by `feather list` and the dashboard.
type RunSummary struct {
	RunID        uuid.UUID
	ConfigName   string
	Status       RunStatus
	Trigger      TriggerKind
	DryRun       bool
	StartedAt    time.Time
	FinishedAt   *time.Time
	DocsMatched  int64
	DocsScanned  int64
	DocsArchived int64
	DocsDeleted  int64
	BytesStored  int64
	Batches      int
	Error        string
}

// Progress is how far through its matched documents this run has archived, as a
// percentage. It reports 0 when the total is unknown — a run from before the
// count was recorded, or a dry run that never got that far — so a caller should
// check DocsMatched before showing a bar.
func (r RunSummary) Progress() int {
	if r.DocsMatched <= 0 {
		return 0
	}
	pct := int(r.DocsArchived * 100 / r.DocsMatched)
	return min(pct, 100)
}

// Duration is how long a run took, or how long it has been going.
func (r RunSummary) Duration() time.Duration {
	if r.FinishedAt != nil {
		return r.FinishedAt.Sub(r.StartedAt)
	}
	return time.Since(r.StartedAt)
}

// ImportStatus is the lifecycle state of one import. The values match the run
// statuses they mean the same thing as, so the UI colours them alike.
type ImportStatus string

const (
	ImportRunning ImportStatus = "running"
	ImportSuccess ImportStatus = "success"
	ImportFailed  ImportStatus = "failed"
	// ImportDryRun is a dry run that read and checked everything. It wrote
	// nothing, which is why it is not "success".
	ImportDryRun ImportStatus = "dry_run"
)

// Where an import's documents came from.
const (
	ImportSourceArchive = "archive"
	ImportSourceRun     = "run"
	ImportSourceUpload  = "upload"
)

// What an import does with a document whose _id is already in the target.
const (
	ImportModeInsert  = "insert"
	ImportModeReplace = "replace"
)

// Import is one request to put archived documents back into MongoDB, and how
// far it got.
type Import struct {
	ID          uuid.UUID
	Actor       string
	ActorIP     string
	SourceKind  string
	SourceRef   string
	SourceLabel string
	PreferAlias string

	TargetAlias      string
	TargetDatabase   string
	TargetCollection string

	Mode       string
	DryRun     bool
	Status     ImportStatus
	StartedAt  time.Time
	FinishedAt *time.Time

	FilesTotal    int
	FilesDone     int
	DocsExpected  int64
	BytesExpected int64
	DocsRead      int64
	DocsInserted  int64
	DocsPresent   int64
	DocsReplaced  int64
	BytesRead     int64
	SHA256        string
	Error         string
}

// Progress is how far through its input this import is, as a percentage.
// Documents are the better measure when the catalogue knows how many to expect;
// an upload only has a size, so it falls back to bytes. 0 means unknown.
func (i Import) Progress() int {
	var pct int64
	switch {
	case i.DocsExpected > 0:
		pct = i.DocsRead * 100 / i.DocsExpected
	case i.BytesExpected > 0:
		pct = i.BytesRead * 100 / i.BytesExpected
	default:
		return 0
	}
	return int(min(pct, 100))
}

// Duration is how long the import took, or how long it has been going.
func (i Import) Duration() time.Duration {
	if i.FinishedAt != nil {
		return i.FinishedAt.Sub(i.StartedAt)
	}
	return time.Since(i.StartedAt)
}

// Target is the namespace written to, as an operator would write it.
func (i Import) Target() string { return i.TargetDatabase + "." + i.TargetCollection }

// Totals is the headline set of numbers on the dashboard.
type Totals struct {
	Configs      int
	EnabledJobs  int
	Destinations int
	MongoSources int
	// PostgresSources counts enabled PostgreSQL accounts.
	PostgresSources int
	Artifacts       int64
	DocsArchived    int64
	DocsDeleted     int64
	BytesStored     int64
	FailedRuns      int64
	RunsLast24h     int64
	LastRunAt       *time.Time
	OldestArchive   *time.Time
}

// --- credentials and configs ----------------------------------------------

// MongoSourceRecord is a stored MongoDB account. The URI is sealed on disk;
// Preview is the redacted form shown in the UI.
type MongoSourceRecord struct {
	ID         string
	Alias      string
	URI        string // decrypted on read
	Preview    string
	AuthSource string
	AppName    string
	Enabled    bool
	Note       string
	Check      CheckResult
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Source converts a stored record into the runtime form the Mongo pool wants.
func (m MongoSourceRecord) Source() config.MongoSource {
	return config.MongoSource{
		Alias:      m.Alias,
		URI:        m.URI,
		AuthSource: m.AuthSource,
		AppName:    m.AppName,
	}
}

// PostgresSourceRecord is a stored PostgreSQL account. The DSN is sealed on
// disk; Preview is the redacted form shown in the UI.
type PostgresSourceRecord struct {
	ID        string
	Alias     string
	DSN       string // decrypted on read
	Preview   string
	AppName   string
	Enabled   bool
	Note      string
	Check     CheckResult
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Source converts a stored record into the runtime form the Postgres pool
// wants.
func (p PostgresSourceRecord) Source() config.PostgresSource {
	return config.PostgresSource{Alias: p.Alias, DSN: p.DSN, AppName: p.AppName}
}

// DestinationRecord is a stored backup destination — one S3 or Oracle account.
type DestinationRecord struct {
	ID        string
	Enabled   bool
	Note      string
	Check     CheckResult
	CreatedAt time.Time
	UpdatedAt time.Time

	config.Destination // credentials decrypted on read
}

// CheckResult remembers the last connectivity test, so the UI can show whether
// an account actually works without re-testing on every page load.
type CheckResult struct {
	At    *time.Time
	OK    bool
	Error string
}

// Tested reports whether a check has ever run.
func (c CheckResult) Tested() bool { return c.At != nil }

// JobRecord is a stored retention config. Config carries the same shape the
// runner has always used, so nothing downstream cares that it came from a table
// rather than a JSON file.
type JobRecord struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	Config    *config.JobConfig

	// Problem is set when this stored config could not be prepared or is not
	// safe to run — a rule added since it was saved, a timezone this host
	// cannot resolve, a hand-edited row. The record is still returned so the
	// operator can see and fix it; it is simply never scheduled or run.
	Problem string
}

// Broken reports whether this config is parked because of Problem.
func (r JobRecord) Broken() bool { return r.Problem != "" }

// AdminUser is someone who may log into the UI.
type AdminUser struct {
	ID           string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// Session is a live browser login.
type Session struct {
	TokenHash string
	UserID    string
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
	IP        string
}
