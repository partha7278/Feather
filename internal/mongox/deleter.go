package mongox

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// DeleteByIDs removes exactly the documents that were archived, in chunks.
//
// The original filter is re-applied alongside the id list. That is the point of
// this function: between the read and the delete a document may have been
// updated so that it no longer matches the retention condition, and such a
// document must survive even though its id is in the archive. The archive then
// holds a copy of a row that still exists, which is harmless; the reverse —
// deleting a row that no longer qualified — is not.
func DeleteByIDs(ctx context.Context, coll *mongo.Collection, filter bson.D, ids []bson.RawValue, chunkSize int) (int64, error) {
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

		result, err := coll.DeleteMany(ctx, guardedFilter(filter, chunk))
		if err != nil {
			return deleted, fmt.Errorf("deleting archived documents: %w", err)
		}
		deleted += result.DeletedCount
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
