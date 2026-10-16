package pgsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Writer puts archived rows back into a table.
//
// Rows go in through json_populate_recordset, which is the inverse of the
// row_to_json they were archived with: Postgres parses each value with the
// column's own input function, so a timestamp, a numeric, a bytea or an array
// comes back exactly as it left.
type Writer struct {
	db    querier
	table *Table

	insert  string // the insert-only statement
	replace string // the upsert, keyed on the primary key
}

// NewWriter prepares to restore into one table.
//
// Generated columns are left out of the insert — Postgres refuses a value for
// one and recomputes it anyway — and OVERRIDING SYSTEM VALUE lets the archived
// value of a GENERATED ALWAYS identity key go back in, which it must: that key
// is how the row is known everywhere else.
func NewWriter(ctx context.Context, db querier, schema, table string) (*Writer, error) {
	described, err := Describe(ctx, db, schema, table)
	if err != nil {
		return nil, err
	}

	var names, updates []string
	for _, col := range described.columns {
		if col.generated {
			continue
		}
		names = append(names, col.ident)
		if col.ident != described.pk {
			updates = append(updates, col.ident+" = EXCLUDED."+col.ident)
		}
	}
	if len(updates) == 0 {
		// A key-only table: there is nothing to replace, but ON CONFLICT DO
		// UPDATE still needs a SET to report the row as touched.
		updates = append(updates, described.pk+" = EXCLUDED."+described.pk)
	}
	list := strings.Join(names, ", ")

	base := fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) OVERRIDING SYSTEM VALUE
		SELECT %[2]s FROM json_populate_recordset(NULL::%[1]s, $1::json)`, described.ident, list)
	return &Writer{
		db:     db,
		table:  described,
		insert: base + ` ON CONFLICT DO NOTHING`,
		// xmax is zero on a freshly inserted row version and set on one that
		// replaced an existing row, which is how the two are told apart.
		replace: base + fmt.Sprintf(` ON CONFLICT (%s) DO UPDATE SET %s RETURNING (xmax = 0)`,
			described.pk, strings.Join(updates, ", ")),
	}, nil
}

// Table is the table being written.
func (w *Writer) Table() *Table { return w.table }

// WriteRows inserts one batch of archived rows, each a JSON object on its own.
// With replace, a row whose key already exists is overwritten; otherwise it is
// left alone and counted as already present. A dry run only checks each line
// is a JSON object.
func (w *Writer) WriteRows(ctx context.Context, lines [][]byte, replace, dryRun bool) (inserted, replaced, present int64, err error) {
	if len(lines) == 0 {
		return 0, 0, 0, nil
	}
	for i, line := range lines {
		if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return 0, 0, 0, fmt.Errorf("row %d of this batch is not a JSON object — this is not a PostgreSQL archive", i+1)
		}
	}
	if dryRun {
		return 0, 0, 0, nil
	}

	payload := make([]byte, 0, len(lines)*128)
	payload = append(payload, '[')
	for i, line := range lines {
		if i > 0 {
			payload = append(payload, ',')
		}
		payload = append(payload, line...)
	}
	payload = append(payload, ']')
	total := int64(len(lines))

	if !replace {
		tag, err := w.db.Exec(ctx, w.insert, string(payload))
		if err != nil {
			return 0, 0, 0, fmt.Errorf("inserting into %s: %w", w.table.ident, err)
		}
		inserted = tag.RowsAffected()
		return inserted, 0, total - inserted, nil
	}

	rows, err := w.db.Query(ctx, w.replace, string(payload))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("restoring into %s: %w", w.table.ident, err)
	}
	defer rows.Close()
	for rows.Next() {
		var fresh bool
		if err := rows.Scan(&fresh); err != nil {
			return inserted, replaced, 0, err
		}
		if fresh {
			inserted++
		} else {
			replaced++
		}
	}
	if err := rows.Err(); err != nil {
		return inserted, replaced, 0, fmt.Errorf("restoring into %s: %w", w.table.ident, err)
	}
	return inserted, replaced, total - inserted - replaced, nil
}
