// Package db connects to Postgres and keeps its schema up to date.
package db

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey identifies this app's migration lock among Postgres
// advisory locks. The value is arbitrary; it only has to be unique within the
// database.
const migrationLockKey int64 = 5_150_251_001

// A migration is one schema change, read from a file named
// <version>_<description>.sql (e.g. 0001_create_pokemon.sql).
type migration struct {
	version int64
	name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^([0-9]+)_[a-z0-9_]+\.sql$`)

// Migrate applies the embedded migrations the database doesn't have yet and
// returns the versions it applied.
//
// During a deploy the old and new versions run side by side against the same
// database, so migrations must only add things (expand): a column or table
// the old version still reads can only be dropped in a later deploy.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]int64, error) {
	migrations, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	return migrate(ctx, pool, migrations)
}

// loadMigrations reads the .sql files in dir, sorted by version. Versions
// must run 1, 2, 3... with no gaps or duplicates.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	paths, err := fs.Glob(fsys, path.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	var migrations []migration
	for _, p := range paths {
		name := path.Base(p)
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration %s: name must look like 0001_description.sql", name)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		sql, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(sql)})
	}
	slices.SortFunc(migrations, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i, m := range migrations {
		if want := int64(i + 1); m.version != want {
			return nil, fmt.Errorf("migration %s: expected version %d next (versions must run 1, 2, 3... with no gaps or duplicates)", m.name, want)
		}
	}
	return migrations, nil
}

// migrate holds an advisory lock for the whole run, so when two versions
// start at once (a deploy), the second waits and then finds nothing to do.
// Each migration runs in its own transaction with its schema_migrations row.
// Migration files must not contain BEGIN/COMMIT or statements that can't run
// in a transaction (CREATE INDEX CONCURRENTLY).
func migrate(ctx context.Context, pool *pgxpool.Pool, migrations []migration) (applied []int64, err error) {
	// Advisory locks belong to a session, so everything runs on one connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("taking the migration lock: %w", err)
	}
	defer unlockAndRelease(ctx, conn)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    bigint PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("creating schema_migrations: %w", err)
	}

	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	// A database ahead of the binary means an older image (a rollback) against
	// a newer schema. That's expected for one deploy's overlap, and fine as
	// long as migrations only add things, so it's allowed rather than fatal:
	// the platform's rollback runs an older image on purpose.
	for _, m := range migrations {
		if slices.Contains(done, m.version) {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments, so pgx uses the simple query protocol, which
			// allows several statements in one file.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", m.name, err)
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}

// unlockAndRelease releases the migration lock and returns conn to the pool.
func unlockAndRelease(ctx context.Context, conn *pgxpool.Conn) {
	// Not ctx itself: if it was cancelled mid-migration, the lock still has
	// to be released.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		// Closing the connection ends the session, which releases the lock;
		// returning it to the pool would keep the lock held.
		_ = conn.Hijack().Close(ctx)
		return
	}
	conn.Release()
}
