package app

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/mongox"
	"github.com/unifers/feather/internal/pgsrc"
)

// CountMatching resolves a config's filter as of now and counts what it
// matches — the number worth knowing before turning deletion on. It returns
// the resolved filter alongside, as the run would record it.
//
// It is the same question for both engines, asked from the CLI and from the
// control panel, so it is answered once, here.
func (a *App) CountMatching(ctx context.Context, job *config.JobConfig, now time.Time) (int64, string, error) {
	if job.Source.IsPostgres() {
		condition, err := job.Source.SQLFilter()
		if err != nil {
			return 0, "", err
		}
		resolved, err := config.ResolveSQLTimeTokens(condition, now)
		if err != nil {
			return 0, "", err
		}
		shown := resolved
		if shown == "" {
			shown = "TRUE"
		}
		db, err := a.Postgres.Get(ctx, job.Source.PostgresSource)
		if err != nil {
			return 0, shown, err
		}
		src, err := pgsrc.Bind(ctx, db, job.Source.Database, job.Source.Collection, resolved, job.Source.TimestampField)
		if err != nil {
			return 0, shown, err
		}
		n, err := src.Count(ctx)
		return n, shown, err
	}

	resolved, err := config.ResolveTimeTokens(job.Source.Filter, now)
	if err != nil {
		return 0, "", err
	}
	var compact bytes.Buffer
	shown := string(resolved)
	if json.Compact(&compact, resolved) == nil {
		shown = compact.String()
	}
	filter, err := mongox.ParseFilter(resolved)
	if err != nil {
		return 0, shown, err
	}
	client, err := a.Mongo.Get(ctx, job.Source.MongoSource)
	if err != nil {
		return 0, shown, err
	}
	coll, err := client.Collection(job.Source.Database, job.Source.Collection, job.Source.ReadPreference)
	if err != nil {
		return 0, shown, err
	}
	n, err := mongox.Count(ctx, coll, filter)
	return n, shown, err
}
