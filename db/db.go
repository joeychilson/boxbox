package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/001_initial_schema.sql
var migrationSQL string

// DB is the database connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// New creates a new database connection pool.
func New(ctx context.Context, databaseURL string) (*DB, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}

	config.ConnConfig.RuntimeParams["search_path"] = "boxbox,public"

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Migrate runs database migrations.
func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, migrationSQL)
	if err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}

// Close closes the database connection pool.
func (db *DB) Close() {
	db.pool.Close()
}
