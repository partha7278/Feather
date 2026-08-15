package mongox

import (
	"context"
	"fmt"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/unifers/feather/internal/retry"
)

// DeleteByIDs removes exactly the documents that were archived, in chunks.
//
// The original filter is re-applied alongside the id list. That is the point of
// this function: between the read and the delete a document may have been
// updated so that it no longer matches the retention condition, and such a
// document must survive even though its id is in the archive. The archive then
// holds a copy of a row that still exists, which is harmless; the reverse —
// deleting a row that no longer qualified — is not.
// A chunk that fails is retried. DeleteMany with an explicit id list is
// idempotent — a document deleted by a first attempt that then lost its
// response simply does not match the second — so the only cost of a retry is
// that the redelivered ids contribute nothing to the count, which is the same
// undercount a lost response produces anyway.
func DeleteByIDs(ctx context.Context, log *slog.Logger, coll *mongo.Collection, filter bson.D, ids []bson.RawValue, chunkSize int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if chunkSize <= 0 {
		chunkSize = 5000
	}

	var deleted int64
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))

		chunk := make(bson.A, 0, end-start)
		for _, id := range ids[start:end] {
			chunk = append(chunk, id)
		}

		err := retry.Do(ctx, log, retry.DefaultPolicy, "delete archived documents", func(ctx context.Context) error {
			result, err := coll.DeleteMany(ctx, guardedFilter(filter, chunk))
			if err != nil {
				return err
			}
			deleted += result.DeletedCount
			return nil
		})
		if err != nil {
			// The count is returned alongside the error on purpose: chunks that
			// already succeeded really are deleted, and the caller has to record
			// that or the catalogue will disagree with MongoDB.
			return deleted, fmt.Errorf("deleting archived documents: %w", err)
		}
	}
	return deleted, nil
}

func guardedFilter(filter bson.D, ids bson.A) bson.D {
	idClause := bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}
	if len(filter) == 0 {
		return idClause
	}
	return bson.D{{Key: "$and", Value: bson.A{filter, idClause}}}
}
