package mongox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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

// Document is one archived record plus the pieces feather needs to catalogue
// and later delete it.
type Document struct {
	Raw bson.Raw
	ID  bson.RawValue
	TS  *time.Time
}

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
		SetNoCursorTimeout(true)

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
		return Document{}, false, fmt.Errorf("document has no _id — feather cannot safely delete it later")
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
