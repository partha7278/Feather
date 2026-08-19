package mongox

import (
	"context"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Source is the collection-shaped surface the runner needs.
//
// It exists so the runner can be tested. The pipeline's central promise —
// nothing is deleted until every copy is stored and verified — was checked by a
// comment and nothing else, because exercising it meant a real MongoDB and a
// real bucket. Behind this interface a test can make an upload fail on demand
// and assert that DeleteByIDs is never reached.
//
// It is deliberately narrow: four operations, the ones a retention run performs.
type Source interface {
	// MaxID is the largest _id matching the filter at this instant, used to
	// fence the run to a reproducible set of documents.
	MaxID(ctx context.Context, filter bson.D) (bson.RawValue, bool, error)

	// Count is how many documents the filter matches.
	Count(ctx context.Context, filter bson.D) (int64, error)

	// OpenCursor streams matching documents in _id order.
	OpenCursor(ctx context.Context, filter, projection bson.D, fetchSize int32, timestampField string) (Stream, error)

	// DeleteByIDs removes documents by id, re-applying the filter so a document
	// that stopped matching is left alone.
	DeleteByIDs(ctx context.Context, log *slog.Logger, filter bson.D, ids []bson.RawValue, chunkSize int) (int64, error)
}

// Stream is an open cursor over matching documents.
type Stream interface {
	Next(ctx context.Context) (Document, bool, error)
	Close(ctx context.Context) error
}

// CollectionSource adapts a real MongoDB collection to Source.
type CollectionSource struct {
	coll *mongo.Collection
}

// NewSource wraps a collection.
func NewSource(coll *mongo.Collection) *CollectionSource {
	return &CollectionSource{coll: coll}
}

func (c *CollectionSource) MaxID(ctx context.Context, filter bson.D) (bson.RawValue, bool, error) {
	return MaxID(ctx, c.coll, filter)
}

func (c *CollectionSource) Count(ctx context.Context, filter bson.D) (int64, error) {
	return Count(ctx, c.coll, filter)
}

func (c *CollectionSource) OpenCursor(ctx context.Context, filter, projection bson.D, fetchSize int32, timestampField string) (Stream, error) {
	return OpenCursor(ctx, c.coll, filter, projection, fetchSize, timestampField)
}

func (c *CollectionSource) DeleteByIDs(ctx context.Context, log *slog.Logger, filter bson.D, ids []bson.RawValue, chunkSize int) (int64, error) {
	return DeleteByIDs(ctx, log, c.coll, filter, ids, chunkSize)
}
