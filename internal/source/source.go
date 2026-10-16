// Package source is the contract between the runner and a database it archives
// from. MongoDB and PostgreSQL each implement it; the runner — and with it the
// ordering that keeps every deletion behind a verified copy — is written once,
// against this, and does not know which engine it is draining.
package source

import (
	"context"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Source is one config's filter bound to one collection or table for the
// length of a run.
//
// Binding the filter in, rather than passing it to every call, is what lets
// the two engines differ underneath: a MongoDB filter is a BSON document and a
// PostgreSQL one is a WHERE clause, and the runner never needs to hold either.
type Source interface {
	// Fence pins the run to the records that match at this instant — in
	// practice, everything up to the highest matching id — so a record written
	// while the run is working is the next run's business. It reports whether
	// anything matched at all. Every later call honours the fence.
	Fence(ctx context.Context) (bool, error)

	// Count is how many records the fenced filter matches.
	Count(ctx context.Context) (int64, error)

	// OpenCursor streams the matching records in id order.
	OpenCursor(ctx context.Context, fetchSize int32) (Stream, error)

	// DeleteByIDs removes records by id, re-applying the filter so one that
	// stopped matching between the read and the delete survives. It returns
	// the number deleted even alongside an error, because chunks that
	// succeeded before the failure really are gone.
	DeleteByIDs(ctx context.Context, log *slog.Logger, ids []bson.RawValue, chunkSize int) (int64, error)
}

// Stream is an open cursor over matching records.
type Stream interface {
	Next(ctx context.Context) (Document, bool, error)
	Close(ctx context.Context) error
}

// Document is one archived record plus the pieces feather needs to catalogue
// and later delete it.
type Document struct {
	// Raw is the record as BSON. The spool writes it as canonical Extended
	// JSON unless Line is set.
	Raw bson.Raw

	// Line, when set, is the record already encoded as one line of JSON, and
	// is written to the archive exactly as given. PostgreSQL rows arrive this
	// way: row_to_json is Postgres's own rendering of the row, which
	// json_populate_record reads back losslessly, and a detour through BSON
	// would only lose precision on the way.
	Line []byte

	// ID is the record's identity — _id for MongoDB, the primary key for
	// PostgreSQL — and the value it is deleted by.
	ID bson.RawValue

	// TS is the record's timestamp field, when the config names one.
	TS *time.Time
}
