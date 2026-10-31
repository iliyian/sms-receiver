// Package store persists received SMS messages in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Record is one persisted SMS message.
type Record struct {
	ID           int64
	DedupeKey    string
	ReceivedAt   time.Time  // server-side receive time (UTC)
	SentAt       *time.Time // best-effort message time reported by the phone (UTC)
	Sender       string
	Content      string
	OrgContent   string
	Device       string
	CardSlot     string
	Title        string
	AppVersion   string
	ReceiveTime  string
	TimestampRaw string
	ClientIP     string
	ContentType  string
	RawBody      string // only populated by Get
}

// ListFilter selects messages for List. Zero values mean "no filter".
type ListFilter struct {
	Limit    int
	BeforeID int64 // return records with id < BeforeID
	Sender   string
	Device   string
	Query    string // substring match on content or sender
	Since    *time.Time
	Until    *time.Time
}

// Store owns the SQLite connection pool.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sms (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	dedupe_key    TEXT    NOT NULL UNIQUE,
	received_ms   INTEGER NOT NULL,
	sent_at       TEXT,
	sender        TEXT    NOT NULL DEFAULT '',
	content       TEXT    NOT NULL DEFAULT '',
	org_content   TEXT    NOT NULL DEFAULT '',
	device        TEXT    NOT NULL DEFAULT '',
	card_slot     TEXT    NOT NULL DEFAULT '',
	title         TEXT    NOT NULL DEFAULT '',
	app_version   TEXT    NOT NULL DEFAULT '',
	receive_time  TEXT    NOT NULL DEFAULT '',
	timestamp_raw TEXT    NOT NULL DEFAULT '',
	client_ip     TEXT    NOT NULL DEFAULT '',
	content_type  TEXT    NOT NULL DEFAULT '',
	raw_body      TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sms_received_ms ON sms(received_ms DESC);
CREATE INDEX IF NOT EXISTS idx_sms_sender_id ON sms(sender, id DESC);
`

// Open creates or opens the SQLite database at path.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}
	if dir := filepath.Dir(abs); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := (&url.URL{Scheme: "file", Path: abs}).String() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// The database holds private messages; keep it readable by the owner only.
	if err := os.Chmod(abs, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod database: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying pool.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Insert stores rec. If a row with the same dedupe key already exists it is
// not written again and duplicate is true.
func (s *Store) Insert(ctx context.Context, rec Record) (id int64, duplicate bool, err error) {
	var sentAt any
	if rec.SentAt != nil {
		sentAt = rec.SentAt.UTC().Format(time.RFC3339Nano)
	}

	const q = `
INSERT INTO sms (dedupe_key, received_ms, sent_at, sender, content, org_content,
                 device, card_slot, title, app_version, receive_time,
                 timestamp_raw, client_ip, content_type, raw_body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dedupe_key) DO NOTHING
RETURNING id`

	err = s.db.QueryRowContext(ctx, q,
		rec.DedupeKey,
		rec.ReceivedAt.UnixMilli(),
		sentAt,
		rec.Sender,
		rec.Content,
		rec.OrgContent,
		rec.Device,
		rec.CardSlot,
		rec.Title,
		rec.AppVersion,
		rec.ReceiveTime,
		rec.TimestampRaw,
		rec.ClientIP,
		rec.ContentType,
		rec.RawBody,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert sms: %w", err)
	}
	return id, false, nil
}

const listColumns = `id, received_ms, sent_at, sender, content, org_content, device,
	card_slot, title, app_version, receive_time, timestamp_raw, client_ip, content_type`

// List returns records newest first, applying f.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Record, error) {
	var where []string
	var args []any

	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	if f.Sender != "" {
		where = append(where, "sender = ?")
		args = append(args, f.Sender)
	}
	if f.Device != "" {
		where = append(where, "device = ?")
		args = append(args, f.Device)
	}
	if f.Query != "" {
		where = append(where, `(content LIKE ? ESCAPE '\' OR sender LIKE ? ESCAPE '\')`)
		pattern := "%" + escapeLike(f.Query) + "%"
		args = append(args, pattern, pattern)
	}
	if f.Since != nil {
		where = append(where, "received_ms >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Until != nil {
		where = append(where, "received_ms < ?")
		args = append(args, f.Until.UnixMilli())
	}

	q := "SELECT " + listColumns + " FROM sms"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sms: %w", err)
	}
	defer rows.Close()

	records := make([]Record, 0, min(f.Limit, 64))
	for rows.Next() {
		rec, err := scanRecord(rows, false)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sms: %w", err)
	}
	return records, nil
}

// Get returns a single record including its raw body, or (nil, nil) if absent.
func (s *Store) Get(ctx context.Context, id int64) (*Record, error) {
	const q = "SELECT " + listColumns + ", raw_body FROM sms WHERE id = ?"
	rec, err := scanRecord(s.db.QueryRowContext(ctx, q, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner, withRaw bool) (Record, error) {
	var (
		rec        Record
		receivedMS int64
		sentAt     sql.NullString
	)
	dest := []any{
		&rec.ID, &receivedMS, &sentAt, &rec.Sender, &rec.Content, &rec.OrgContent,
		&rec.Device, &rec.CardSlot, &rec.Title, &rec.AppVersion, &rec.ReceiveTime,
		&rec.TimestampRaw, &rec.ClientIP, &rec.ContentType,
	}
	if withRaw {
		dest = append(dest, &rec.RawBody)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, sql.ErrNoRows
		}
		return Record{}, fmt.Errorf("scan sms: %w", err)
	}

	rec.ReceivedAt = time.UnixMilli(receivedMS).UTC()
	if sentAt.Valid && sentAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, sentAt.String); err == nil {
			t = t.UTC()
			rec.SentAt = &t
		}
	}
	return rec, nil
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
