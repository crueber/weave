// Package db wraps SQLite (pure Go, WAL) with embedded migrations.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migFS embed.FS

type DB struct{ *sql.DB }

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Writes are serialized by SQLite (IMMEDIATE tx + busy_timeout); allow readers to fan out.
	sdb.SetMaxOpenConns(8)
	d := &DB{sdb}
	return d, d.migrate()
}

func (d *DB) migrate() error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	ents, _ := migFS.ReadDir("migrations")
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		var x string
		if d.QueryRow(`SELECT name FROM schema_migrations WHERE name=?`, n).Scan(&x) == nil {
			continue
		}
		b, _ := migFS.ReadFile("migrations/" + n)
		err := d.Tx(context.Background(), func(tx *sql.Tx) error {
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
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
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

func IsNoRows(err error) bool { return err == sql.ErrNoRows }
func IsUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}
