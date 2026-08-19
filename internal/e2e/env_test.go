//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongooptions "go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/secrets"
	"github.com/unifers/feather/internal/store"
)

const (
	testDatabase = "feather_e2e"
	// brokenAlias points at a bucket that does not exist, for the test that a
	// failing destination deletes nothing.
	brokenAlias = "s3-broken"
)

// env is one isolated end-to-end fixture: its own SQLite database, its own
// MongoDB collection, and the rig's three buckets emptied before each test.
type env struct {
	t          *testing.T
	rig        Rig
	app        *app.App
	mongo      *mongo.Client
	s3         *s3.Client
	collection string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	rig := requireRig(t)

	client, err := mongo.Connect(mongooptions.Client().ApplyURI(rig.MongoURI))
	if err != nil {
		t.Fatalf("connecting to the rig's MongoDB: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("the rig's MongoDB is not answering: %v", err)
	}

	e := &env{
		t: t, rig: rig, mongo: client,
		s3: newS3Client(t, rig),
		// One collection per test, so tests cannot see each other's documents.
		collection: fmt.Sprintf("orders_%d", time.Now().UnixNano()),
	}
	e.emptyBuckets()
	t.Cleanup(func() {
		_ = client.Database(testDatabase).Collection(e.collection).Drop(context.Background())
		_ = client.Database(testDatabase).Collection("orders_restored").Drop(context.Background())
		_ = client.Database(testDatabase).Collection("orders_from_bad_copy").Drop(context.Background())
	})

	e.app = e.openApp()
	return e
}

// openApp builds a real feather over a throwaway database, with the rig's
// accounts registered the way the control panel would register them.
func (e *env) openApp() *app.App {
	e.t.Helper()

	dataDir := e.t.TempDir()
	key, err := secrets.GenerateKeyHex()
	if err != nil {
		e.t.Fatal(err)
	}

	env := &config.Env{
		DataDir:           dataDir,
		DBPath:            filepath.Join(dataDir, "feather.db"),
		SpoolDir:          filepath.Join(dataDir, "spool"),
		MasterKey:         key,
		MaxConcurrentJobs: 2,
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}

	a, err := app.Open(context.Background(), env, log)
	if err != nil {
		e.t.Fatalf("opening feather: %v", err)
	}
	e.t.Cleanup(func() { a.Close(context.Background()) })

	ctx := context.Background()
	source := &store.MongoSourceRecord{Alias: "rig", URI: e.rig.MongoURI, AuthSource: "admin", AppName: "feather-e2e", Enabled: true}
	if err := a.Store.SaveMongoSource(ctx, source); err != nil {
		e.t.Fatalf("saving the MongoDB account: %v", err)
	}

	for _, spec := range []struct {
		alias, bucket, provider, prefix string
	}{
		{"s3-primary", "feather-primary", "s3", ""},
		{"s3-backup", "feather-backup", "s3", "dr"},
		{"oci-cold", "feather-cold", "oci", ""},
		// Deliberately nonexistent, for the broken-destination test.
		{brokenAlias, "feather-does-not-exist", "s3", ""},
	} {
		record := &store.DestinationRecord{Enabled: true}
		record.Alias = spec.alias
		record.Provider = config.Provider(spec.provider)
		record.Bucket = spec.bucket
		record.Region = "us-east-1"
		record.Endpoint = e.rig.S3Endpoint
		record.AccessKeyID = e.rig.S3AccessKey
		record.SecretAccessKey = e.rig.S3SecretKey
		record.ForcePathStyle = true
		record.Prefix = spec.prefix
		if err := a.Store.SaveDestination(ctx, record); err != nil {
			e.t.Fatalf("saving destination %s: %v", spec.alias, err)
		}
	}

	if err := a.Reload(ctx); err != nil {
		e.t.Fatalf("reloading: %v", err)
	}
	return a
}

// config builds a retention config over this test's collection.
func (e *env) config(t *testing.T, name string, deletes bool) *config.JobConfig {
	t.Helper()

	verify := true
	job := &config.JobConfig{
		Name: name,
		Source: config.Source{
			MongoSource: "rig",
			Database:    testDatabase,
			Collection:  e.collection,
			// Everything seeded is older than this.
			Filter: json.RawMessage(`{"placedAt":{"$lt":{"$date":"2030-01-01T00:00:00Z"}}}`),
		},
		Destinations: []string{"s3-primary", "s3-backup", "oci-cold"},
		Output:       config.Output{KeyTemplate: "archive/{db}/{collection}/{config}-{runId}-{seq}{ext}"},
		Compression:  config.Compression{Codec: config.CodecGzip},
		Schedule:     config.Schedule{Every: "24h"},
		Retention:    config.Retention{DeleteAfterArchive: deletes, VerifyBeforeDelete: &verify},
		Batch:        config.Batch{MaxDocs: 750},
	}
	if err := job.Prepare(); err != nil {
		t.Fatalf("preparing the config: %v", err)
	}

	record := &store.JobRecord{Config: job}
	if err := e.app.Store.SaveJob(context.Background(), record); err != nil {
		t.Fatalf("saving the config: %v", err)
	}
	return job
}

// seed writes documents covering the types the archive format promises to
// preserve, and returns them by id for comparison after a restore.
func (e *env) seed(t *testing.T, count int) map[string]bson.Raw {
	t.Helper()

	collection := e.mongo.Database(testDatabase).Collection(e.collection)
	originals := make(map[string]bson.Raw, count)
	docs := make([]any, 0, count)

	for i := range count {
		decimal, err := bson.ParseDecimal128(fmt.Sprintf("%d.%02d", i, i%100))
		if err != nil {
			t.Fatalf("parsing a decimal: %v", err)
		}
		doc := bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "placedAt", Value: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)},
			{Key: "seq", Value: int32(i)},
			{Key: "customerId", Value: 9007199254740993 + int64(i)},
			{Key: "total", Value: decimal},
			{Key: "note", Value: fmt.Sprintf("café ☕ order %d", i)},
			{Key: "tags", Value: bson.A{"a", int32(1), true}},
		}
		docs = append(docs, doc)

		encoded, err := bson.Marshal(doc)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		id, err := bson.Raw(encoded).LookupErr("_id")
		if err != nil {
			t.Fatal(err)
		}
		originals[id.String()] = encoded
	}

	if _, err := collection.InsertMany(context.Background(), docs); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return originals
}

// count is how many documents still match the retention filter.
func (e *env) count(t *testing.T) int64 {
	t.Helper()
	filter := bson.D{{Key: "placedAt", Value: bson.D{{Key: "$lt", Value: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}}}
	total, err := e.mongo.Database(testDatabase).Collection(e.collection).CountDocuments(context.Background(), filter)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	return total
}

// readAll returns every document in a collection, by id.
func (e *env) readAll(t *testing.T, collection string) map[string]bson.Raw {
	t.Helper()

	cursor, err := e.mongo.Database(testDatabase).Collection(collection).Find(context.Background(), bson.D{})
	if err != nil {
		t.Fatalf("reading %s: %v", collection, err)
	}
	defer func() { _ = cursor.Close(context.Background()) }()

	out := map[string]bson.Raw{}
	for cursor.Next(context.Background()) {
		raw := make(bson.Raw, len(cursor.Current))
		copy(raw, cursor.Current)
		id, err := raw.LookupErr("_id")
		if err != nil {
			t.Fatal(err)
		}
		out[id.String()] = raw
	}
	return out
}

// --- object storage helpers ---

func newS3Client(t *testing.T, rig Rig) *s3.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			rig.S3AccessKey, rig.S3SecretKey, "")))
	if err != nil {
		t.Fatalf("building an S3 client: %v", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(rig.S3Endpoint)
		o.UsePathStyle = true
	})
}

func (e *env) emptyBuckets() {
	e.t.Helper()
	for _, bucket := range e.rig.Buckets {
		for _, key := range e.list(e.t, bucket) {
			_, _ = e.s3.DeleteObject(context.Background(), &s3.DeleteObjectInput{
				Bucket: aws.String(bucket), Key: aws.String(key),
			})
		}
	}
}

func (e *env) list(t *testing.T, bucket string) []string {
	t.Helper()

	var keys []string
	paginator := s3.NewListObjectsV2Paginator(e.s3, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			t.Fatalf("listing %s: %v", bucket, err)
		}
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}
	return keys
}

func (e *env) object(t *testing.T, bucket, key string) []byte {
	t.Helper()

	out, err := e.s3.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucket, key, err)
	}
	return body
}

func (e *env) putObject(t *testing.T, bucket, key string, body []byte) {
	t.Helper()

	if _, err := e.s3.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(string(body)),
	}); err != nil {
		t.Fatalf("writing %s/%s: %v", bucket, key, err)
	}
}
