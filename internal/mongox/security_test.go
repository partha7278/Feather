package mongox

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
)

const uriPassword = "Mongo-Pw-7731"

// closedPort is an address nothing listens on, so a connection attempt fails
// without any MongoDB being involved.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// hostileURIs are connection strings an operator might type, each carrying a
// password, each guaranteed to fail. Connect's error is returned verbatim by
// Pool.Get to the collection picker's JSON API and by Ping to the account
// test, so it must not carry the password whatever went wrong.
func hostileURIs(t *testing.T) map[string]string {
	addr := closedPort(t)
	return map[string]string{
		"a non-numeric port":        "mongodb://app:" + uriPassword + "@db.invalid:notaport/",
		"an unknown option":         "mongodb://app:" + uriPassword + "@" + addr + "/?bogusOption=1&authSource=admin",
		"an unknown auth mechanism": "mongodb://app:" + uriPassword + "@" + addr + "/?authMechanism=NOPE",
		"an unescaped slash":        "mongodb://app:" + uriPassword + "/x@" + addr + "/",
		"an unescaped at sign":      "mongodb://app:" + uriPassword + "@x@" + addr + "/",
		"a bad scheme":              "mongo://app:" + uriPassword + "@" + addr + "/",
		"an unreachable server":     "mongodb://app:" + uriPassword + "@" + addr + "/?connectTimeoutMS=200&serverSelectionTimeoutMS=200",
		"srv with a port":           "mongodb+srv://app:" + uriPassword + "@cluster.invalid:27017/",
		"a bad tls option":          "mongodb://app:" + uriPassword + "@" + addr + "/?tls=maybe",
	}
}

func TestConnectErrorsNeverCarryThePassword(t *testing.T) {
	for name, uri := range hostileURIs(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			client, err := Connect(ctx, uri, "admin", "feather-test")
			if err == nil {
				_ = client.Close(context.Background())
				t.Fatal("connected to something that should not exist")
			}
			if strings.Contains(err.Error(), uriPassword) {
				t.Errorf("the error carries the password: %v", err)
			}
		})
	}
}

// Ping is how the UI tests an account before saving it, and Pool.Get feeds the
// database and collection pickers. Both hand their error to the browser.
func TestPingAndPoolErrorsNeverCarryThePassword(t *testing.T) {
	for name, uri := range hostileURIs(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			source := config.MongoSource{Alias: "prod", URI: uri, AuthSource: "admin", AppName: "feather-test"}
			if err := Ping(ctx, source); err == nil {
				t.Fatal("ping succeeded against something that should not exist")
			} else if strings.Contains(err.Error(), uriPassword) {
				t.Errorf("Ping's error carries the password: %v", err)
			}

			pool := NewPool(map[string]config.MongoSource{"prod": source})
			defer pool.Close(context.Background())
			if _, err := pool.Get(ctx, "prod"); err == nil {
				t.Fatal("the pool connected to something that should not exist")
			} else if strings.Contains(err.Error(), uriPassword) {
				t.Errorf("Pool.Get's error carries the password: %v", err)
			}
		})
	}
}

// An unknown alias is reported by name; the pool must not reveal what the
// configured accounts are or what they connect to.
func TestPoolRefusesAnUnknownAliasWithoutRevealingOthers(t *testing.T) {
	pool := NewPool(map[string]config.MongoSource{
		"prod": {Alias: "prod", URI: "mongodb://app:" + uriPassword + "@db:27017"},
	})
	_, err := pool.Get(t.Context(), "../prod")
	if err == nil {
		t.Fatal("an unknown alias resolved")
	}
	if strings.Contains(err.Error(), uriPassword) || strings.Contains(err.Error(), "db:27017") {
		t.Errorf("the error reveals another account: %v", err)
	}
}

// The pool keeps its own copy of the accounts it was given. A caller mutating
// its map afterwards — or a concurrent edit in the UI — must not redirect an
// alias to a different cluster behind the pool's back.
func TestThePoolIsNotRedirectedByMutatingTheCallersMap(t *testing.T) {
	sources := map[string]config.MongoSource{"prod": {Alias: "prod", URI: "mongodb://a@trusted:27017"}}
	pool := NewPool(sources)
	sources["prod"] = config.MongoSource{Alias: "prod", URI: "mongodb://a@attacker:27017"}
	sources["extra"] = config.MongoSource{Alias: "extra"}

	if got := pool.sources["prod"].URI; got != "mongodb://a@trusted:27017" {
		t.Errorf("the pool now points at %s", got)
	}
	if len(pool.Aliases()) != 1 {
		t.Errorf("the pool picked up an alias added after construction: %v", pool.Aliases())
	}
}

// The delete re-applies the retention filter beside the id list. However the
// operator's filter is written — with its own _id clause, its own $or, or a
// $nor that matches everything — the id list must still bound what is deleted.
func TestTheGuardedDeleteFilterIsAlwaysBoundedByTheIDList(t *testing.T) {
	ids := bson.A{bson.NewObjectID(), bson.NewObjectID()}
	filters := map[string]string{
		"empty":                   `{}`,
		"its own _id clause":      `{"_id":{"$exists":true}}`,
		"an $or that matches all": `{"$or":[{"a":1},{"a":{"$exists":false}}]}`,
		"an $and of its own":      `{"$and":[{"_id":{"$gt":{"$oid":"000000000000000000000000"}}}]}`,
		"a $nor that matches all": `{"$nor":[{"never":"present"}]}`,
		"a top-level $in on _id":  `{"_id":{"$in":[1,2,3]}}`,
	}
	for name, raw := range filters {
		t.Run(name, func(t *testing.T) {
			filter, err := ParseFilter(json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			guarded := guardedFilter(filter, ids)

			idClause := bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}
			if len(filter) == 0 {
				if !equalBSON(t, guarded, idClause) {
					t.Fatalf("an empty filter did not reduce to the id list: %v", guarded)
				}
				return
			}
			if len(guarded) != 1 || guarded[0].Key != "$and" {
				t.Fatalf("the guarded filter is not a single $and: %v", guarded)
			}
			clauses, ok := guarded[0].Value.(bson.A)
			if !ok || len(clauses) != 2 {
				t.Fatalf("the $and does not hold exactly the filter and the id list: %v", guarded)
			}
			if !equalBSON(t, clauses[1], idClause) {
				t.Errorf("the id clause was altered: %v", clauses[1])
			}
		})
	}
}

func equalBSON(t *testing.T, a, b any) bool {
	t.Helper()
	ea, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: a}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: b}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(ea) == string(eb)
}

// Filters arrive from stored configs. (Trailing data after the first document is
// not caught here — config.Prepare's json.Valid check refuses it upstream.)
// Input that is not a single JSON document
// must be refused, not partially applied — a filter silently reduced to {}
// would match, archive and delete the whole collection.
func TestParseFilterRefusesInputThatIsNotOneDocument(t *testing.T) {
	for _, raw := range []string{
		`[{"a":1}]`,
		`"a string"`,
		`{"a":1`,
		`{"a":{"$date":"not a date"}}`,
		strings.Repeat(`{"a":`, 10_000) + `1` + strings.Repeat(`}`, 10_000),
	} {
		short := raw
		if len(short) > 40 {
			short = short[:40] + "…"
		}
		filter, err := ParseFilter(json.RawMessage(raw))
		if err == nil {
			t.Errorf("%s parsed to %v", short, filter)
		}
	}
}
