package verify

import (
	"context"
	"database/sql"
	"errors"
)

// Engine is the query surface a data check needs. It exists so checks are
// written once and work against any restored engine: SQL databases go
// through database/sql, document stores through their own shell.
//
// Query strings are engine-dialect — SQL for postgres/mysql, a mongosh
// expression for mongodb — exactly as the backup tooling is.
type Engine interface {
	// Count evaluates a query returning a single numeric value.
	Count(ctx context.Context, query string) (int64, error)
	// Rows evaluates a query and reports how many rows/documents it returned.
	Rows(ctx context.Context, query string) (int, error)
	// Scalar evaluates a query returning a single string value.
	Scalar(ctx context.Context, query string) (string, error)
	// Checksum computes an order-independent checksum over one column/field.
	// Identifiers are validated by the caller before they get here.
	Checksum(ctx context.Context, table, column string) (string, error)
}

// errNoChecksumDialect is returned by engines with no checksum support wired.
var errNoChecksumDialect = errors.New("no checksum dialect configured")

// sqlEngine runs checks against a restored SQL database.
type sqlEngine struct {
	db *sql.DB
	// checksumQuery builds the engine-dialect checksum query.
	checksumQuery func(table, column string) string
}

// NewSQL wraps a database/sql handle as an Engine. A nil handle yields a nil
// Engine so checks report "no connection" rather than panicking.
func NewSQL(db *sql.DB, checksumQuery func(table, column string) string) Engine {
	if db == nil {
		return nil
	}
	return &sqlEngine{db: db, checksumQuery: checksumQuery}
}

func (e *sqlEngine) Count(ctx context.Context, query string) (int64, error) {
	if e.db == nil {
		return 0, errors.New("no database connection")
	}
	var n int64
	if err := e.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (e *sqlEngine) Rows(ctx context.Context, query string) (int, error) {
	if e.db == nil {
		return 0, errors.New("no database connection")
	}
	rows, err := e.db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

func (e *sqlEngine) Scalar(ctx context.Context, query string) (string, error) {
	if e.db == nil {
		return "", errors.New("no database connection")
	}
	var s string
	if err := e.db.QueryRowContext(ctx, query).Scan(&s); err != nil {
		return "", err
	}
	return s, nil
}

func (e *sqlEngine) Checksum(ctx context.Context, table, column string) (string, error) {
	if e.checksumQuery == nil {
		return "", errNoChecksumDialect
	}
	if e.db == nil {
		return "", errors.New("no database connection")
	}
	var sum string
	if err := e.db.QueryRowContext(ctx, e.checksumQuery(table, column)).Scan(&sum); err != nil {
		return "", err
	}
	return sum, nil
}
