// Package database manages the PostgreSQL connection pool used by the
// repository layer.
package database

import (
    "context"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
)

// Connect creates a pgx connection pool, applies sane lifetime settings, and
// verifies connectivity with a bounded ping. The caller is responsible for
// calling Close on the returned pool.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
    if strings.TrimSpace(databaseURL) == "" {
        return nil, errors.New("database URL is empty")
    }

    poolConfig, err := pgxpool.ParseConfig(databaseURL)
    if err != nil {
        return nil, fmt.Errorf("parse database URL: %w", err)
    }

    poolConfig.MaxConnLifetime = 30 * time.Minute
    poolConfig.MaxConnIdleTime = 5 * time.Minute
    poolConfig.HealthCheckPeriod = time.Minute

    // Defensive server-side guards: no query may hang the request forever.
    if poolConfig.ConnConfig != nil {
        poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
        poolConfig.ConnConfig.RuntimeParams["application_name"] = "inkwell"
    }

    pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
    if err != nil {
        return nil, fmt.Errorf("create connection pool: %w", err)
    }

    pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    if err := pool.Ping(pingCtx); err != nil {
        pool.Close()
        return nil, fmt.Errorf("database ping: %w", err)
    }

    return pool, nil
}

// Ping verifies the pool can reach PostgreSQL within the context deadline.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
    return pool.Ping(ctx)
}
