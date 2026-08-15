package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// JobConfig is one retention job: what to archive, where to put it, when to
// run, and whether the source documents go afterwards. It lives in the
// database and is edited through the UI; credentials never appear in it, only
// the aliases of accounts stored separately.
type JobConfig struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`

	Source       Source      `json:"source"`
	Batch        Batch       `json:"batch"`
	Compression  Compression `json:"compression"`
	Output       Output      `json:"output"`
	Destinations []string    `json:"destinations"`
	Retention    Retention   `json:"retention"`
	Schedule     Schedule    `json:"schedule"`
	Limits       Limits      `json:"limits"`
	DryRun       bool        `json:"dryRun"`

	// Populated by the loader.
	SourcePath string `json:"-"`
	SHA256     string `json:"-"`
}

type Source struct {
	// MongoSource names the stored MongoDB account to read from. Several may be
	// configured, so a single feather can serve clusters that share nothing.
	MongoSource    string          `json:"mongoSource"`
	Database       string          `json:"database"`
	Collection     string          `json:"collection"`
	Filter         json.RawMessage `json:"filter"`
	Projection     json.RawMessage `json:"projection,omitempty"`
	ReadPreference string          `json:"readPreference"`
	TimestampField string          `json:"timestampField"`
}

// Catalog is the set of accounts a job may refer to, used to check that its
// aliases resolve to something that actually exists.
type Catalog struct {
	Destinations map[string]Destination
	Sources      map[string]MongoSource
}

type Batch struct {
	MaxDocs   int      `json:"maxDocs"`
	MaxBytes  ByteSize `json:"maxBytes"`
	FetchSize int32    `json:"fetchSize"`
}

type Compression struct {
	Codec string `json:"codec"`
	Level int    `json:"level"`
}

type Output struct {
	Format      string `json:"format"`
	KeyTemplate string `json:"keyTemplate"`
}

type Retention struct {
	DeleteAfterArchive bool  `json:"deleteAfterArchive"`
	VerifyBeforeDelete *bool `json:"verifyBeforeDelete"`
	DeleteBatchSize    int   `json:"deleteBatchSize"`
}

type Schedule struct {
	Cron     string `json:"cron"`
	Every    string `json:"every"`
	At       string `json:"at"`
	Timezone string `json:"timezone"`

	// Resolved during validation.
	EveryDuration time.Duration  `json:"-"`
	AtHour        int            `json:"-"`
	AtMinute      int            `json:"-"`
	Location      *time.Location `json:"-"`
}

type Limits struct {
	MaxDocsPerRun int      `json:"maxDocsPerRun"`
	MaxRuntime    Duration `json:"maxRuntime"`
}

const (
	CodecGzip = "gzip"
	CodecZstd = "zstd"

	FormatNDJSON = "ndjson"

	DefaultKeyTemplate = "feather/{db}/{collection}/{yyyy}/{MM}/{dd}/{config}-{runId}-{seq}{ext}"
)

// IsEnabled reports whether the daemon should schedule this job. Absent means
// enabled — a config file exists to be run.
func (c *JobConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// ShouldVerify reports whether uploads must be re-read before any delete.
// Absent means yes; turning it off is an explicit choice.
func (c *JobConfig) ShouldVerify() bool {
	return c.Retention.VerifyBeforeDelete == nil || *c.Retention.VerifyBeforeDelete
}

// Extension is the file suffix implied by the format and codec pair.
func (c *JobConfig) Extension() string {
	base := ".ndjson"
	switch c.Compression.Codec {
	case CodecZstd:
		return base + ".zst"
	default:
		return base + ".gz"
	}
}

// applyDefaults fills in everything the operator did not specify. Defaults are
// chosen to be safe on a small VPS: modest batches, no unbounded memory use.
func (c *JobConfig) applyDefaults() {
	if c.Source.ReadPreference == "" {
		c.Source.ReadPreference = "primary"
	}
	if len(c.Source.Filter) == 0 {
		c.Source.Filter = json.RawMessage(`{}`)
	}
	// A written-out "projection": null reads back as the four bytes "null",
	// which is valid JSON and would be stored and sent to Mongo as a projection.
	// "No projection" has to stay absent.
	if strings.TrimSpace(string(c.Source.Projection)) == "null" {
		c.Source.Projection = nil
	}
	if c.Batch.MaxDocs <= 0 {
		c.Batch.MaxDocs = 50_000
	}
	if c.Batch.MaxBytes <= 0 {
		c.Batch.MaxBytes = 256 << 20
	}
	if c.Batch.FetchSize <= 0 {
		c.Batch.FetchSize = 1000
	}
	if c.Compression.Codec == "" {
		c.Compression.Codec = CodecGzip
	}
	if c.Compression.Level == 0 {
		c.Compression.Level = 6
	}
	if c.Output.Format == "" {
		c.Output.Format = FormatNDJSON
	}
	if c.Output.KeyTemplate == "" {
		c.Output.KeyTemplate = DefaultKeyTemplate
	}
	if c.Retention.DeleteBatchSize <= 0 {
		c.Retention.DeleteBatchSize = 5000
	}
	if c.Schedule.Timezone == "" {
		c.Schedule.Timezone = "UTC"
	}
	if c.Limits.MaxRuntime <= 0 {
		c.Limits.MaxRuntime = Duration(6 * time.Hour)
	}
}

var (
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	atRe   = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
)

// Validate checks a job config and resolves derived fields. It is deliberately
// strict: feather refuses a bad config rather than running a half-understood
// retention job against production data.
func (c *JobConfig) Validate(known Catalog) error {
	if err := c.Prepare(); err != nil {
		return err
	}

	if c.Source.MongoSource == "" {
		return fmt.Errorf("source.mongoSource is required — name the MongoDB account to read from")
	}
	if known.Sources != nil {
		if _, ok := known.Sources[c.Source.MongoSource]; !ok {
			return fmt.Errorf("source.mongoSource %q is not a configured MongoDB account", c.Source.MongoSource)
		}
	}
	for _, alias := range c.Destinations {
		if known.Destinations == nil {
			break
		}
		if _, ok := known.Destinations[alias]; !ok {
			return fmt.Errorf("destination %q is not a configured backup account", alias)
		}
	}
	return nil
}

// Prepare fills in defaults and resolves derived fields — schedule location,
// interval, byte sizes — checking everything that can be judged without
// knowing which accounts exist. Loading a stored config goes through here;
// saving one goes through Validate, which also checks its aliases resolve.
func (c *JobConfig) Prepare() error {
	c.applyDefaults()

	if !nameRe.MatchString(c.Name) {
		return fmt.Errorf("name %q must match %s", c.Name, nameRe)
	}
	if c.Source.Database == "" {
		return fmt.Errorf("source.database is required")
	}
	if c.Source.Collection == "" {
		return fmt.Errorf("source.collection is required")
	}
	if !json.Valid(c.Source.Filter) {
		return fmt.Errorf("source.filter is not valid JSON")
	}
	if len(c.Source.Projection) > 0 && !json.Valid(c.Source.Projection) {
		return fmt.Errorf("source.projection is not valid JSON")
	}
	// An empty filter plus deletion would archive and then wipe the whole
	// collection. That is almost never intended, so it must be spelled out.
	if isEmptyFilter(c.Source.Filter) && c.Retention.DeleteAfterArchive && c.Limits.MaxDocsPerRun <= 0 {
		return fmt.Errorf("source.filter is empty while retention.deleteAfterArchive is true — that would delete the entire collection; set a filter or a limits.maxDocsPerRun cap")
	}

	switch strings.ToLower(c.Source.ReadPreference) {
	case "primary", "primarypreferred", "secondary", "secondarypreferred", "nearest":
	default:
		return fmt.Errorf("source.readPreference %q is not a valid MongoDB read preference", c.Source.ReadPreference)
	}

	switch c.Output.Format {
	case FormatNDJSON:
	default:
		return fmt.Errorf("output.format %q is unsupported (only %q)", c.Output.Format, FormatNDJSON)
	}

	switch c.Compression.Codec {
	case CodecGzip:
		if c.Compression.Level < 1 || c.Compression.Level > 9 {
			return fmt.Errorf("compression.level %d is out of range for gzip (1-9)", c.Compression.Level)
		}
	case CodecZstd:
		if c.Compression.Level < 1 || c.Compression.Level > 4 {
			return fmt.Errorf("compression.level %d is out of range for zstd (1=fastest .. 4=best)", c.Compression.Level)
		}
	default:
		return fmt.Errorf("compression.codec %q is unsupported (want %q or %q)", c.Compression.Codec, CodecGzip, CodecZstd)
	}

	if len(c.Destinations) == 0 {
		return fmt.Errorf("at least one destination is required — feather will not delete data it has not stored somewhere")
	}
	seen := map[string]bool{}
	for _, alias := range c.Destinations {
		if seen[alias] {
			return fmt.Errorf("destination %q is listed twice", alias)
		}
		seen[alias] = true
	}

	if err := ValidateKeyTemplate(c.Output.KeyTemplate); err != nil {
		return fmt.Errorf("output.keyTemplate: %w", err)
	}
	if !strings.Contains(c.Output.KeyTemplate, "{runId}") && !strings.Contains(c.Output.KeyTemplate, "{seq}") {
		return fmt.Errorf("output.keyTemplate must contain {runId} or {seq} so batches do not overwrite each other")
	}

	if err := c.Schedule.resolve(); err != nil {
		return err
	}

	if _, err := ResolveTimeTokens(c.Source.Filter, time.Now()); err != nil {
		return fmt.Errorf("source.filter: %w", err)
	}

	return nil
}

func (s *Schedule) resolve() error {
	if s.Cron == "" && s.Every == "" {
		return fmt.Errorf("schedule needs either \"cron\" or \"every\"")
	}
	if s.Cron != "" && s.Every != "" {
		return fmt.Errorf("schedule has both \"cron\" and \"every\" — pick one")
	}

	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return fmt.Errorf("schedule.timezone %q: %w", s.Timezone, err)
	}
	s.Location = loc
	s.AtHour, s.AtMinute = -1, -1

	if s.Every == "" {
		return nil
	}

	d, err := ParseDuration(s.Every)
	if err != nil {
		return fmt.Errorf("schedule.every %q: %w", s.Every, err)
	}
	if d < time.Minute {
		return fmt.Errorf("schedule.every %q is below the one minute minimum", s.Every)
	}
	s.EveryDuration = d

	if s.At == "" {
		return nil
	}
	m := atRe.FindStringSubmatch(s.At)
	if m == nil {
		return fmt.Errorf("schedule.at %q must be HH:MM in 24-hour form", s.At)
	}
	// "at" pins a wall-clock time, which only means something for intervals of
	// a day or more; below that the interval alone decides.
	if d < 24*time.Hour {
		return fmt.Errorf("schedule.at is only meaningful when schedule.every is at least 24h")
	}
	s.AtHour, _ = strconv.Atoi(m[1])
	s.AtMinute, _ = strconv.Atoi(m[2])
	return nil
}

// ComputeSHA fingerprints the config's meaningful contents. Every run records
// the fingerprint of the config it ran, so a change to a job can be tied to the
// runs that happened before and after it.
func (c *JobConfig) ComputeSHA() string {
	encoded, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// PreviewKey renders the file name template against a plausible run, so the UI
// can show what archives will actually be called before anything is saved.
func (c *JobConfig) PreviewKey(at time.Time) string {
	return RenderKey(c.Output.KeyTemplate, KeyVars{
		Database:   orPlaceholder(c.Source.Database, "database"),
		Collection: orPlaceholder(c.Source.Collection, "collection"),
		Config:     orPlaceholder(c.Name, "config"),
		RunID:      "0f5b1c9a-3d4e-4a1b-9c8d-7e6f5a4b3c2d",
		Seq:        1,
		Extension:  c.Extension(),
		At:         at,
	})
}

func orPlaceholder(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func isEmptyFilter(raw json.RawMessage) bool {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return len(m) == 0
}

// ByteSize parses human sizes like "256MB" or a plain byte count.
type ByteSize int64

var byteSizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]?I?B?)$`)

func (b *ByteSize) UnmarshalJSON(data []byte) error {
	var asNumber int64
	if err := json.Unmarshal(data, &asNumber); err == nil {
		*b = ByteSize(asNumber)
		return nil
	}

	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return fmt.Errorf("byte size must be a number or a string like \"256MB\"")
	}

	parsed, err := ParseByteSize(asString)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// ParseByteSize reads a human size like "256MB", "1.5GiB" or a plain byte
// count. The UI and the JSON importer both go through here, so a size typed in
// a form means exactly what the same string means in a config file.
func ParseByteSize(raw string) (ByteSize, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == "" {
		return 0, fmt.Errorf("a size is required, e.g. 256MB")
	}

	m := byteSizeRe.FindStringSubmatch(trimmed)
	if m == nil {
		return 0, fmt.Errorf("cannot parse size %q (try a form like 256MB or 1GB)", raw)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse size %q: %w", raw, err)
	}

	multiplier := float64(1)
	switch strings.TrimSuffix(strings.TrimSuffix(m[2], "B"), "I") {
	case "K":
		multiplier = 1 << 10
	case "M":
		multiplier = 1 << 20
	case "G":
		multiplier = 1 << 30
	case "T":
		multiplier = 1 << 40
	}
	return ByteSize(value * multiplier), nil
}

func (b ByteSize) String() string { return HumanBytes(int64(b)) }

// Duration accepts Go duration strings plus a "d" suffix for days.
type Duration time.Duration

// MarshalJSON writes the string form its own reader expects, so a config
// feather produces can be read back by feather.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("duration must be a string like \"6h\" or \"2d\"")
	}
	parsed, err := ParseDuration(raw)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

var dayRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)d$`)

// ParseDuration extends time.ParseDuration with a day unit, since retention
// windows are naturally expressed in days.
func ParseDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if m := dayRe.FindStringSubmatch(raw); m != nil {
		days, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("cannot parse duration %q: %w", raw, err)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("cannot parse duration %q (want forms like 30m, 6h, 2d)", raw)
	}
	return d, nil
}

// HumanBytes renders a byte count for logs and CLI output.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
