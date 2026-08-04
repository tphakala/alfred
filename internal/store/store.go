package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps a PostgreSQL connection pool.
type Store struct {
	pool *pgxpool.Pool
	url  string
}

// IsNotFound returns true if the error wraps pgx.ErrNoRows.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// IsConflict returns true if the error is a PostgreSQL unique_violation (23505).
func IsConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// New creates a new Store, opening a pgxpool connection and verifying connectivity.
// maxConns configures the pool's maximum number of connections; 0 means use the
// pgxpool default.
func New(ctx context.Context, databaseURL string, maxConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Store{pool: pool, url: databaseURL}, nil
}

// Close shuts down the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping verifies database connectivity by pinging the underlying connection pool.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Pool returns the underlying pgxpool.Pool for use by sub-repositories.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Migrate runs all pending up-migrations embedded in the binary.
// It is idempotent: calling it when the schema is already current returns nil.
func (s *Store) Migrate() error {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}

	// golang-migrate's pgx/v5 driver expects the pgx5:// scheme.
	migrateURL := strings.Replace(s.url, "postgres://", "pgx5://", 1)
	migrateURL = strings.Replace(migrateURL, "postgresql://", "pgx5://", 1)

	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}
