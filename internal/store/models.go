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
}

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
	DocsScanned  int64
	DocsArchived int64
	DocsDeleted  int64
	BytesStored  int64
	Batches      int
	Error        string
}

// Duration is how long a run took, or how long it has been going.
func (r RunSummary) Duration() time.Duration {
	if r.FinishedAt != nil {
		return r.FinishedAt.Sub(r.StartedAt)
	}
	return time.Since(r.StartedAt)
}

// Totals is the headline set of numbers on the dashboard.
type Totals struct {
	Configs       int
	EnabledJobs   int
	Destinations  int
	MongoSources  int
	Artifacts     int64
	DocsArchived  int64
	DocsDeleted   int64
	BytesStored   int64
	FailedRuns    int64
	RunsLast24h   int64
	LastRunAt     *time.Time
	OldestArchive *time.Time
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
}

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
