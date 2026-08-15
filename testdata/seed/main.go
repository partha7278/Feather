// Command seed fills the local rig's Mongo with orders spanning six months, so
// a retention filter of "older than 90 days" matches a known subset.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func main() {
	uri := flag.String("uri", "mongodb://127.0.0.1:27018", "mongo uri")
	dbName := flag.String("db", "shop", "database")
	collName := flag.String("collection", "orders", "collection")
	total := flag.Int("n", 200_000, "documents to insert")
	payload := flag.Int("payload", 0, "bytes of incompressible random data per document")
	flag.Parse()

	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(*uri))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(ctx)

	coll := client.Database(*dbName).Collection(*collName)
	if err := coll.Drop(ctx); err != nil {
		log.Fatal(err)
	}

	rng := rand.New(rand.NewSource(42))
	statuses := []string{"completed", "pending", "cancelled"}
	now := time.Now().UTC()

	const chunk = 5000
	inserted := 0
	for inserted < *total {
		size := min(chunk, *total-inserted)
		docs := make([]any, 0, size)

		for range size {
			// Spread evenly over 180 days so roughly half fall outside a
			// 90-day retention window.
			age := time.Duration(rng.Intn(180*24)) * time.Hour
			createdAt := now.Add(-age)
			amount, _ := bson.ParseDecimal128(fmt.Sprintf("%d.%02d", rng.Intn(5000), rng.Intn(100)))

			// Random bytes defeat the compressor, which is how the rig produces
			// archives large enough to exercise the multipart upload path.
			blob := ""
			if *payload > 0 {
				raw := make([]byte, *payload)
				rng.Read(raw)
				blob = base64.StdEncoding.EncodeToString(raw)
			}

			docs = append(docs, bson.D{
				{Key: "_id", Value: bson.NewObjectID()},
				{Key: "blob", Value: blob},
				{Key: "status", Value: statuses[rng.Intn(len(statuses))]},
				{Key: "createdAt", Value: bson.NewDateTimeFromTime(createdAt)},
				{Key: "customer", Value: fmt.Sprintf("cust-%06d", rng.Intn(50_000))},
				{Key: "amount", Value: amount},
				{Key: "items", Value: bson.A{
					bson.D{{Key: "sku", Value: fmt.Sprintf("SKU-%04d", rng.Intn(2000))}, {Key: "qty", Value: rng.Intn(5) + 1}},
				}},
				{Key: "notes", Value: "lorem ipsum dolor sit amet, consectetur adipiscing elit"},
			})
		}

		if _, err := coll.InsertMany(ctx, docs); err != nil {
			log.Fatal(err)
		}
		inserted += size
		fmt.Printf("\rinserted %d/%d", inserted, *total)
	}
	fmt.Println()

	cutoff := now.AddDate(0, 0, -90)
	matching, err := coll.CountDocuments(ctx, bson.D{
		{Key: "status", Value: "completed"},
		{Key: "createdAt", Value: bson.D{{Key: "$lt", Value: bson.NewDateTimeFromTime(cutoff)}}},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("total=%d  matching 'completed older than 90d'=%d\n", inserted, matching)
}
