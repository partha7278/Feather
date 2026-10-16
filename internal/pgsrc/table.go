package pgsrc

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier is the part of a pool or connection this package uses.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Table is what feather needs to know about a table before touching it. Every
// name in it is already quoted, so it can be spliced into a statement as is.
type Table struct {
	Schema string
	Name   string

	ident  string // "schema"."table"
	pk     string // "id"
	pkName string // id, unquoted
	// pkType is the key's type as format_type renders it — "bigint", "uuid",
	// "character varying(64)" — used to cast text parameters back to it.
	// Keys travel as text because that is the one form every key type has.
	pkType string

	columns []column
}

type column struct {
	name      string // unquoted
	ident     string // quoted
	generated bool   // GENERATED ALWAYS AS (...) STORED: cannot be inserted
}

// Ident is the quoted, schema-qualified table name.
func (t *Table) Ident() string { return t.ident }

// PrimaryKey is the unquoted name of the key column.
func (t *Table) PrimaryKey() string { return t.pkName }

func (t *Table) column(name string) (column, bool) {
	for _, c := range t.columns {
		if c.name == name {
			return c, true
		}
	}
	return column{}, false
}

// Describe looks a table up in the catalogue and checks feather can work with
// it: it exists, and it has a primary key of exactly one column.
func Describe(ctx context.Context, q querier, schema, name string) (*Table, error) {
	table := &Table{
		Schema: schema,
		Name:   name,
		ident:  pgx.Identifier{schema, name}.Sanitize(),
	}

	rows, err := q.Query(ctx, `
		SELECT a.attname, a.attgenerated <> '', COALESCE(a.attnum = ANY (i.indkey), false),
		       format_type(a.atttypid, a.atttypmod),
		       COALESCE(array_length(i.indkey, 1), 0)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_catalog.pg_index i ON i.indrelid = c.oid AND i.indisprimary
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p')
		ORDER BY a.attnum`, schema, name)
	if err != nil {
		return nil, fmt.Errorf("reading the definition of %s: %w", table.ident, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			col        column
			isKey      bool
			typeName   string
			keyColumns int
		)
		if err := rows.Scan(&col.name, &col.generated, &isKey, &typeName, &keyColumns); err != nil {
			return nil, err
		}
		col.ident = pgx.Identifier{col.name}.Sanitize()
		table.columns = append(table.columns, col)
		if isKey {
			if keyColumns > 1 {
				return nil, fmt.Errorf("%s has a composite primary key (%d columns); Feather needs a single-column key to page through and delete by", table.ident, keyColumns)
			}
			table.pk, table.pkName, table.pkType = col.ident, col.name, typeName
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the definition of %s: %w", table.ident, err)
	}

	if len(table.columns) == 0 {
		return nil, fmt.Errorf("table %s does not exist, or this account cannot see it", table.ident)
	}
	if table.pk == "" {
		return nil, fmt.Errorf("%s has no primary key; Feather needs one to page through the table and to delete exactly the rows it archived", table.ident)
	}
	return table, nil
}

// Schemas lists the schemas this account can see, leaving out Postgres's own.
func Schemas(ctx context.Context, q querier) ([]string, error) {
	return strings1(ctx, q, `
		SELECT nspname FROM pg_catalog.pg_namespace
		WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
		  AND has_schema_privilege(oid, 'USAGE')
		ORDER BY nspname`)
}

// Tables lists one schema's tables. Partitions are left out: a partitioned
// table is archived through its parent.
func Tables(ctx context.Context, q querier, schema string) ([]string, error) {
	return strings1(ctx, q, `
		SELECT c.relname FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		ORDER BY c.relname`, schema)
}

func strings1(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return out, nil
}
