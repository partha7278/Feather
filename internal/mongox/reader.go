package mongox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/unifers/feather/internal/source"
)

// ParseFilter turns a resolved Extended JSON filter into a BSON document.
// Operators such as $lt sit alongside type wrappers such as $date, and the
// driver's Extended JSON parser handles both.
func ParseFilter(raw json.RawMessage) (bson.D, error) {
	if len(raw) == 0 {
		return bson.D{}, nil
	}
	var filter bson.D
	if err := bson.UnmarshalExtJSON(raw, false, &filter); err != nil {
		return nil, fmt.Errorf("parsing filter as MongoDB Extended JSON: %w", err)
	}
	return filter, nil
}

// ParseProjection turns an optional projection into a BSON document.
func ParseProjection(raw json.RawMessage) (bson.D, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var projection bson.D
	if err := bson.UnmarshalExtJSON(raw, false, &projection); err != nil {
		return nil, fmt.Errorf("parsing projection: %w", err)
	}
	return projection, nil
}

// Document is one archived record. It is the engine-neutral type; the alias
// keeps the MongoDB code reading as it always has.
type Document = source.Document

// Cursor streams matching documents in _id order. Sorting by _id gives a stable
// traversal that is cheap on every collection (the index always exists) and
// makes each batch describe a contiguous, resumable id range.
type Cursor struct {
	cursor         *mongo.Cursor
	timestampField string
}

func OpenCursor(ctx context.Context, coll *mongo.Collection, filter, projection bson.D, fetchSize int32, timestampField string) (*Cursor, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetBatchSize(fetchSize).
		// Archive runs are long; the default 10-minute idle cursor timeout would
		// kill a run that spends its time uploading rather than reading.
		SetNoCursorTimeout(true).
		// The sort is on _id, which every collection indexes, so the planner
		// normally satisfies it from that index. But a more selective index on
		// a filtered field can win, and then the sort becomes a blocking stage
		// bounded by MongoDB's 100 MB in-memory limit — which shows up as a
		// mysterious mid-run failure on exactly the large collections feather
		// exists for.
		SetAllowDiskUse(true)

	if projection != nil {
		// _id is required: it is how the documents get deleted afterwards.
		opts.SetProjection(ensureIDProjected(projection))
	}

	cursor, err := coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("opening cursor on %s.%s: %w", coll.Database().Name(), coll.Name(), err)
	}
	return &Cursor{cursor: cursor, timestampField: timestampField}, nil
}

// MaxID reports the largest _id currently matching the filter, and whether
// there was one at all.
//
// A run reads through a live cursor, not a snapshot, so a document inserted
// while the run is working — matching the filter, with an _id ahead of where
// the cursor has reached — is picked up and, if deletion is on, deleted. For
// ObjectId ids and an "older than 90 days" filter that cannot happen, because
// new ids sort last and new documents do not match. For custom or UUID ids, a
// backdated import, or any filter over a mutable field, it can.
//
// Fencing the run at the largest id present when it started makes the set it
// covers well defined: everything that matched at the moment the run began, and
// nothing that arrived afterwards. Anything newer is simply the next run's
// work, which is what an operator reading "archives documents older than 90
// days" already assumes.
func MaxID(ctx context.Context, coll *mongo.Collection, filter bson.D) (bson.RawValue, bool, error) {
	opts := options.FindOne().
		SetSort(bson.D{{Key: "_id", Value: -1}}).
		SetProjection(bson.D{{Key: "_id", Value: 1}})

	var doc bson.Raw
	err := coll.FindOne(ctx, filter, opts).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return bson.RawValue{}, false, nil
	}
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("finding the highest matching _id in %s.%s: %w",
			coll.Database().Name(), coll.Name(), err)
	}

	id, err := doc.LookupErr("_id")
	if err != nil {
		return bson.RawValue{}, false, fmt.Errorf("the highest matching document in %s.%s has no _id",
			coll.Database().Name(), coll.Name())
	}
	// Detached from the document buffer: see archive.copyRawValue for why.
	value := make([]byte, len(id.Value))
	copy(value, id.Value)
	return bson.RawValue{Type: id.Type, Value: value}, true, nil
}

// FenceAtID returns the filter with an upper bound on _id added, so a run only
// sees documents that already existed when it started.
func FenceAtID(filter bson.D, maxID bson.RawValue) bson.D {
	fence := bson.D{{Key: "_id", Value: bson.D{{Key: "$lte", Value: maxID}}}}
	if len(filter) == 0 {
		return fence
	}
	return bson.D{{Key: "$and", Value: bson.A{filter, fence}}}
}

// Next advances the cursor. A false return with a nil error means the result
// set is exhausted.
func (c *Cursor) Next(ctx context.Context) (Document, bool, error) {
	if !c.cursor.Next(ctx) {
		if err := c.cursor.Err(); err != nil {
			return Document{}, false, fmt.Errorf("reading cursor: %w", err)
		}
		return Document{}, false, nil
	}

	raw := make(bson.Raw, len(c.cursor.Current))
	copy(raw, c.cursor.Current)

	id, err := raw.LookupErr("_id")
	if err != nil {
		return Document{}, false, fmt.Errorf("document has no _id — Feather cannot safely delete it later")
	}

	doc := Document{Raw: raw, ID: id}
	if c.timestampField != "" {
		if v, err := raw.LookupErr(c.timestampField); err == nil {
			if t, ok := v.TimeOK(); ok {
				utc := t.UTC()
				doc.TS = &utc
			}
		}
	}
	return doc, true, nil
}

func (c *Cursor) Close(ctx context.Context) error { return c.cursor.Close(ctx) }

// Count reports how many documents the filter matches, for dry runs and logs.
func Count(ctx context.Context, coll *mongo.Collection, filter bson.D) (int64, error) {
	return coll.CountDocuments(ctx, filter)
}

func ensureIDProjected(projection bson.D) bson.D {
	for _, elem := range projection {
		if elem.Key == "_id" {
			return projection
		}
	}
	return append(bson.D{{Key: "_id", Value: 1}}, projection...)
}
