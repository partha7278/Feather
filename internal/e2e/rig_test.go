//go:build integration

package e2e

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// TestRigIsReachable fails fast and legibly when the rig is up but wrong,
// rather than letting every later test fail with a connection error apiece.
func TestRigIsReachable(t *testing.T) {
	rig := requireRig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(rig.MongoURI))
	if err != nil {
		t.Fatalf("connecting to the rig's MongoDB at %s: %v", rig.MongoURI, err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.WithoutCancel(ctx)) })

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		t.Fatalf("pinging the rig's MongoDB at %s: %v", rig.MongoURI, err)
	}
}
