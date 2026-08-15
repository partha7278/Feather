package mongox

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// Client wraps the Mongo driver with the settings feather needs: retryable
// reads, no retryable writes (deletes are already chunked and idempotent), and
// a short server selection timeout so a dead replica fails the run fast.
type Client struct {
	client *mongo.Client
}

func Connect(ctx context.Context, uri, authSource, appName string) (*Client, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetAppName(appName).
		SetRetryReads(true).
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
