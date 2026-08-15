package mongox

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// Client wraps the Mongo driver with the settings feather needs.
//
// These were previously described here and not applied. They are applied now:
//
//   - Retryable reads, because a long archive read should survive a failover.
//   - Retryable writes off. feather's own retry wraps DeleteMany, and the id
//     list makes it idempotent, so the driver retrying underneath as well only
//     obscures which layer did what.
//   - A server selection timeout of 15s rather than the default 30, so a dead
//     replica set fails the run inside a batch instead of a minute later.
//   - A pool sized for the work: a handful of concurrent jobs, each with one
//     cursor and one delete in flight. The default of 100 is a lot of idle
//     connections for a small VPS to hold open against a shared cluster.
//   - No client-wide operation timeout. It would apply to the archive cursor
//     too, and a batch read can legitimately take minutes; the run's own
//     MaxRuntime is the bound that belongs here.
type Client struct {
	client *mongo.Client
}

const (
	serverSelectionTimeout = 15 * time.Second
	connectTimeout         = 15 * time.Second
	maxPoolSize            = 16
)

func Connect(ctx context.Context, uri, authSource, appName string) (*Client, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetAppName(appName).
		SetRetryReads(true).
		SetRetryWrites(false).
		SetServerSelectionTimeout(serverSelectionTimeout).
		SetConnectTimeout(connectTimeout).
		SetMaxPoolSize(maxPoolSize).
		SetCompressors([]string{"zstd", "snappy", "zlib"})

	// Only fill in the auth source when the URI actually carries credentials.
	// Setting it unconditionally would force authentication on an unauthenticated
	// deployment, and would override an authSource already given in the URI.
	if authSource != "" && opts.Auth != nil && opts.Auth.AuthSource == "" {
		opts.Auth.AuthSource = authSource
	}

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connecting to mongo: %w", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("pinging mongo: %w", err)
	}
	return &Client{client: client}, nil
}

func (c *Client) Close(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

// Collection resolves a collection with the given read preference applied.
func (c *Client) Collection(database, collection, readPreference string) (*mongo.Collection, error) {
	rp, err := parseReadPreference(readPreference)
	if err != nil {
		return nil, err
	}
	return c.client.Database(database, options.Database().SetReadPreference(rp)).Collection(collection), nil
}

// Databases lists the databases this account can see, so the UI can offer them
// instead of asking an operator to type a name from memory.
func (c *Client) Databases(ctx context.Context) ([]string, error) {
	names, err := c.client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	out := names[:0]
	for _, name := range names {
		// The server's own bookkeeping databases are never retention targets.
		switch name {
		case "admin", "local", "config":
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Collections lists one database's collections.
func (c *Client) Collections(ctx context.Context, database string) ([]string, error) {
	names, err := c.client.Database(database).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listing collections in %s: %w", database, err)
	}
	sort.Strings(names)
	return names, nil
}

func parseReadPreference(name string) (*readpref.ReadPref, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "primary":
		return readpref.Primary(), nil
	case "primarypreferred":
		return readpref.PrimaryPreferred(), nil
	case "secondary":
		return readpref.Secondary(), nil
	case "secondarypreferred":
		return readpref.SecondaryPreferred(), nil
	case "nearest":
		return readpref.Nearest(), nil
	default:
		return nil, fmt.Errorf("unknown read preference %q", name)
	}
}
