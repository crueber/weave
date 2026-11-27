// Package db wraps SQLite (pure Go, WAL) with embedded migrations.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migFS embed.FS

type DB struct{ *sql.DB }

// sqliteFileURI escapes characters that have special meaning in SQLite file: URIs.
func sqliteFileURI(path string) string {
	return "file:" + strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*DB, error) {
	dsn := sqliteFileURI(path) + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Writes are serialized by SQLite (IMMEDIATE tx + busy_timeout); allow readers to fan out.
	sdb.SetMaxOpenConns(8)
	d := &DB{sdb}
	if err := d.migrate(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return d, nil
}

// OpenReadOnly opens an existing database for reading only: no migrations, no journal-mode change, no file
// creation, and every connection is query_only. It is for host tools that inspect a deployed data directory
// before an upgrade (`weave admin preflight`), possibly while the older server is still running. When the
// directory is not writable (a read-only volume, so SQLite cannot create the WAL index), it falls back to
// immutable=1, which reads the main file as it is: stop the server first in that case or the last committed
// writes still in the WAL are not seen.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	var last error
	for _, extra := range []string{"", "&immutable=1"} {
		dsn := sqliteFileURI(path) + "?mode=ro" + extra + "&_pragma=busy_timeout(10000)&_pragma=query_only(1)"
		sdb, err := sql.Open("sqlite", dsn)
		if err != nil {
			last = err
			continue
		}
		var n int
		if err = sdb.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err != nil {
			sdb.Close()
			last = err
			continue
		}
		sdb.SetMaxOpenConns(2)
		if extra != "" {
			fmt.Fprintf(os.Stderr, "weave: warning: %s opened as immutable (read-only directory, %v): changes still in the WAL are not seen; stop the server first for an exact report\n", path, last)
		}
		return &DB{sdb}, nil
	}
	return nil, last
}

func (d *DB) migrate() error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := d.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	ents, _ := migFS.ReadDir("migrations")
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if applied[n] {
			continue
		}
		b, err := migFS.ReadFile("migrations/" + n)
		if err != nil {
			return fmt.Errorf("migration %s: %w", n, err)
		}
		err = d.Tx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(string(b)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO schema_migrations VALUES(?,?)`, n, Now())
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", n, err)
		}
	}
	return nil
}

// Tx runs fn in a write transaction (BEGIN IMMEDIATE).
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func Now() string { return time.Now().UTC().Format(time.RFC3339) }

var (
	idMu        sync.Mutex
	idMs, idSeq int64
)

// NewID returns a unique id that sorts in creation order: 48-bit ms timestamp,
// 24-bit per-ms sequence (monotonic within the process), 48 random bits (hex).
func NewID() string {
	idMu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= idMs {
		ms = idMs
		idSeq++
	} else {
		idMs, idSeq = ms, 0
	}
	seq := idSeq
	idMu.Unlock()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%012x%06x%s", ms, seq, hex.EncodeToString(b[:]))
}

// Token returns n random bytes hex-encoded.
func Token(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }

// PositionGap is the spacing used for REAL positions; rebalance when gaps < 1.
const PositionGap = 65536.0

func IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
func IsUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}
