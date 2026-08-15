package mongox

import (
	"encoding/json"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A run reads through a live cursor, not a snapshot. Without an upper bound on
// _id, a document inserted mid-run that matches the filter and sorts ahead of
// the cursor is archived and — if deletion is on — deleted, even though it
// arrived after the operator's retention window was decided.
func TestFenceAtIDBoundsTheRun(t *testing.T) {
	maxID := rawObjectID(t, bson.NewObjectID())

	t.Run("adds the bound to an existing filter without disturbing it", func(t *testing.T) {
		filter := bson.D{{Key: "status", Value: "completed"}}
		fenced := FenceAtID(filter, maxID)

		if len(fenced) != 1 || fenced[0].Key != "$and" {
			t.Fatalf("fenced filter is %v, want a single $and", fenced)
		}
		clauses, ok := fenced[0].Value.(bson.A)
		if !ok || len(clauses) != 2 {
			t.Fatalf("$and holds %v", fenced[0].Value)
		}
		// The original filter is preserved as written: rewriting an operator's
		// filter rather than wrapping it is how a retention condition quietly
		// stops meaning what it says.
		if got, ok := clauses[0].(bson.D); !ok || len(got) != 1 || got[0].Key != "status" {
			t.Errorf("the original filter was altered: %v", clauses[0])
		}
	})

	t.Run("an empty filter becomes just the bound", func(t *testing.T) {
		fenced := FenceAtID(bson.D{}, maxID)
		if len(fenced) != 1 || fenced[0].Key != "_id" {
			t.Fatalf("fenced empty filter is %v", fenced)
		}
	})

	t.Run("the bound is inclusive", func(t *testing.T) {
		// $lte, not $lt: the document holding the maximum id matched when the
		// run started and belongs to this run, not the next one.
		fenced := FenceAtID(bson.D{}, maxID)
		clause, ok := fenced[0].Value.(bson.D)
		if !ok || len(clause) != 1 || clause[0].Key != "$lte" {
			t.Fatalf("the id bound is %v, want $lte", fenced[0].Value)
		}
	})
}

func TestParseFilterAcceptsExtendedJSON(t *testing.T) {
	// The type wrappers are the reason a plain JSON parser will not do: $date
	// and $oid have to survive as BSON types, or the filter silently stops
	// matching anything.
	raw := json.RawMessage(`{"createdAt":{"$lt":{"$date":"2026-01-01T00:00:00Z"}},"status":"completed"}`)

	filter, err := ParseFilter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(filter) != 2 {
		t.Fatalf("filter parsed to %v", filter)
	}

	if _, err := ParseFilter(json.RawMessage(`{not json`)); err == nil {
		t.Error("a malformed filter was accepted")
	}
	empty, err := ParseFilter(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("an absent filter should parse to an empty document, got %v (%v)", empty, err)
	}
}

func rawObjectID(t *testing.T, id bson.ObjectID) bson.RawValue {
	t.Helper()
	raw, err := bson.Marshal(bson.M{"_id": id})
	if err != nil {
		t.Fatal(err)
	}
	value, err := bson.Raw(raw).LookupErr("_id")
	if err != nil {
		t.Fatal(err)
	}
	return value
}
