package pgsrc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/retry"
	"github.com/unifers/feather/internal/source"
)

// TableSource is one table with one run's filter applied.
type TableSource struct {
	db     querier
	table  *Table
	filter string // resolved and linted; empty means every row
	ts     string // quoted timestamp column, or empty

	fenced bool
	fence  string // the highest matching key when the run began, as text
}

var _ source.Source = (*TableSource)(nil)

// Bind looks the table up and applies a resolved filter to it.
//
// The filter is linted again here even though the config was linted when it
// was saved. This is the last point before it is spliced into a DELETE, and
// the check costs nothing next to the cost of it being wrong.
func Bind(ctx context.Context, db querier, schema, table, resolvedFilter, timestampField string) (*TableSource, error) {
	if err := config.LintSQLFilter(resolvedFilter); err != nil {
		return nil, fmt.Errorf("source.filter: %w", err)
	}
	described, err := Describe(ctx, db, schema, table)
	if err != nil {
		return nil, err
	}

	src := &TableSource{db: db, table: described, filter: strings.TrimSpace(resolvedFilter)}
	if timestampField != "" {
		col, ok := described.column(timestampField)
		if !ok {
			return nil, fmt.Errorf("the timestamp field %q is not a column of %s", timestampField, described.ident)
		}
		src.ts = col.ident
	}
	return src, nil
}

// where renders the run's condition — the filter, then the fence — appending
// its parameters to args so it can follow parameters the caller already has.
//
// The filter sits alone inside its own parentheses, on its own lines. It has
// been linted, so it cannot close them early; the newlines are there so that a
// reader of pg_stat_activity can see where the operator's text starts and ends.
func (s *TableSource) where(args *[]any) string {
	var parts []string
	if s.filter != "" {
		parts = append(parts, "(\n"+s.filter+"\n)")
	}
	if s.fenced {
		*args = append(*args, s.fence)
		parts = append(parts, fmt.Sprintf("%s <= $%d::text::%s", s.table.pk, len(*args), s.table.pkType))
	}
	if len(parts) == 0 {
		return "TRUE"
	}
	return strings.Join(parts, " AND ")
}

// Fence records the highest key matching the filter right now. Everything the
// run reads, counts and deletes is bounded by it, so a row inserted mid-run —
// or one updated into matching — belongs to the next run.
func (s *TableSource) Fence(ctx context.Context) (bool, error) {
	var args []any
	// The output column gets a name of its own. Left as "id", ORDER BY "id"
	// would bind to the output column — the text cast — and sort "99" above
	// "250", fencing the run far short of what matched.
	query := fmt.Sprintf(`SELECT %s::text AS feather_key FROM %s WHERE %s ORDER BY %s DESC LIMIT 1`,
		s.table.pk, s.table.ident, s.where(&args), s.table.pk)

	var top string
	err := s.db.QueryRow(ctx, query, args...).Scan(&top)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("finding the highest matching %s in %s: %w", s.table.pkName, s.table.ident, err)
	}
	s.fence, s.fenced = top, true
	return true, nil
}

// Count is how many rows the fenced filter matches.
func (s *TableSource) Count(ctx context.Context) (int64, error) {
	var args []any
	var n int64
	query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, s.table.ident, s.where(&args))
	if err := s.db.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting matching rows in %s: %w", s.table.ident, err)
	}
	return n, nil
}

// OpenCursor streams matching rows in key order, a page at a time.
func (s *TableSource) OpenCursor(_ context.Context, fetchSize int32) (source.Stream, error) {
	if fetchSize <= 0 {
		fetchSize = 1000
	}
	return &rowStream{src: s, fetch: int(fetchSize)}, nil
}

// DeleteByIDs removes exactly the archived rows, in chunks.
//
// The statement carries three conditions: the key is one of this batch's, the
// filter still holds, and the key is under the fence. The second is the one
// that matters — a row updated between the read and the delete so that it no
// longer qualifies must survive, even though a copy of it is in the archive.
// That copy is harmless; deleting a row that stopped qualifying is not.
//
// Each chunk is its own statement, so its own transaction. A chunk retried
// after a lost response deletes nothing the first attempt did not, because the
// rows it names are gone: the delete is idempotent, and the only cost is the
// same undercount a lost response causes anyway.
func (s *TableSource) DeleteByIDs(ctx context.Context, log *slog.Logger, ids []bson.RawValue, chunkSize int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if chunkSize <= 0 {
		chunkSize = 5000
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		key, ok := id.StringValueOK()
		if !ok {
			return 0, fmt.Errorf("archived key %s is not text — this batch did not come from a PostgreSQL table", id)
		}
		keys[i] = key
	}

	var deleted int64
	for start := 0; start < len(keys); start += chunkSize {
		chunk := keys[start:min(start+chunkSize, len(keys))]

		args := []any{chunk}
		query := fmt.Sprintf(`DELETE FROM %s WHERE %s = ANY ($1::text[]::%s[]) AND %s`,
			s.table.ident, s.table.pk, s.table.pkType, s.where(&args))

		err := retry.Do(ctx, log, retry.DefaultPolicy, "delete archived rows", func(ctx context.Context) error {
			tag, err := s.db.Exec(ctx, query, args...)
			if err != nil {
				return classify(err)
			}
			deleted += tag.RowsAffected()
			return nil
		})
		if err != nil {
			// The count goes back with the error: earlier chunks really are
			// deleted, and the catalogue has to say so.
			return deleted, fmt.Errorf("deleting archived rows from %s: %w", s.table.ident, err)
		}
	}
	return deleted, nil
}

// rowStream pages through the table by key. Each page is one statement in its
// own implicit transaction; nothing is held open between pages, so a run that
// spends an hour uploading holds no snapshot while it does.
type rowStream struct {
	src   *TableSource
	fetch int

	after    string
	started  bool
	buffer   []source.Document
	finished bool
}

func (r *rowStream) Next(ctx context.Context) (source.Document, bool, error) {
	if len(r.buffer) == 0 {
		if r.finished {
			return source.Document{}, false, nil
		}
		if err := r.page(ctx); err != nil {
			return source.Document{}, false, err
		}
		if len(r.buffer) == 0 {
			return source.Document{}, false, nil
		}
	}
	doc := r.buffer[0]
	r.buffer[0] = source.Document{}
	r.buffer = r.buffer[1:]
	return doc, true, nil
}

func (r *rowStream) Close(context.Context) error {
	r.buffer, r.finished = nil, true
	return nil
}

// page reads the next fetch rows past the last key seen.
//
// The row is rendered with row_to_json on a subquery aliased feather_row
// rather than on the table itself, so the operator's filter can still name
// the table's columns, qualified or not, and so a column that happens to share
// the table's name cannot be mistaken for the whole-row reference.
func (r *rowStream) page(ctx context.Context) error {
	s := r.src
	var args []any
	where := s.where(&args)
	if r.started {
		args = append(args, r.after)
		where += fmt.Sprintf(" AND %s > $%d::text::%s", s.table.pk, len(args), s.table.pkType)
	}
	args = append(args, r.fetch)

	ts := "NULL::timestamptz"
	if s.ts != "" {
		ts = "feather_row." + s.ts + "::timestamptz"
	}
	query := fmt.Sprintf(`
		SELECT row_to_json(feather_row)::text, feather_row.%[1]s::text, %[2]s
		FROM (SELECT * FROM %[3]s WHERE %[4]s ORDER BY %[1]s LIMIT $%[5]d) AS feather_row
		ORDER BY feather_row.%[1]s`,
		s.table.pk, ts, s.table.ident, where, len(args))

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("reading %s: %w", s.table.ident, err)
	}
	defer rows.Close()

	r.buffer = r.buffer[:0]
	for rows.Next() {
		var (
			// Scanned as a string, which pgx always copies; a []byte target
			// may alias the connection's read buffer, and these outlive it.
			line  string
			key   string
			stamp pgtype.Timestamptz
		)
		if err := rows.Scan(&line, &key, &stamp); err != nil {
			return fmt.Errorf("reading %s: %w", s.table.ident, err)
		}

		idType, idValue, err := bson.MarshalValue(key)
		if err != nil {
			return err
		}
		doc := source.Document{
			Line: oneLine([]byte(line)),
			ID:   bson.RawValue{Type: idType, Value: idValue},
		}
		if stamp.Valid && stamp.InfinityModifier == pgtype.Finite {
			at := stamp.Time.UTC()
			doc.TS = &at
		}
		r.buffer = append(r.buffer, doc)
		r.after = key
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", s.table.ident, err)
	}

	r.started = true
	if len(r.buffer) < r.fetch {
		r.finished = true
	}
	return nil
}

// oneLine makes a row safe for a line-per-record archive.
//
// row_to_json embeds a json column's text verbatim, whitespace and all, so a
// pretty-printed value would split one row across several lines. Inside valid
// JSON a raw newline can only be whitespace — one inside a string must be
// escaped — so replacing it with a space changes no value. It is the one
// difference a restore can show: a json (not jsonb) column, which keeps its
// text verbatim, comes back with those newlines as spaces.
func oneLine(line []byte) []byte {
	if bytes.IndexByte(line, '\n') < 0 && bytes.IndexByte(line, '\r') < 0 {
		return line
	}
	out := bytes.ReplaceAll(line, []byte{'\n'}, []byte{' '})
	return bytes.ReplaceAll(out, []byte{'\r'}, []byte{' '})
}
