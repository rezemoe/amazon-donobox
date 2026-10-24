package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS giftcards (
	id          TEXT PRIMARY KEY,
	code        TEXT NOT NULL UNIQUE,
	state       TEXT NOT NULL,
	message     TEXT NOT NULL DEFAULT '',
	attempts    INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	redeemed_at TEXT
);
CREATE INDEX IF NOT EXISTS giftcards_state ON giftcards (state, created_at);

CREATE TABLE IF NOT EXISTS giftcard_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	giftcard_id TEXT NOT NULL REFERENCES giftcards (id),
	state       TEXT NOT NULL,
	message     TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS giftcard_events_card ON giftcard_events (giftcard_id, id);
`

// Fixed-width so timestamps sort correctly as text.
const timeFormat = "2006-01-02T15:04:05.000000Z"

type GiftCard struct {
	ID         string
	Code       string
	State      string
	Message    string
	Attempts   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	RedeemedAt *time.Time
}

type Event struct {
	State     string
	Message   string
	CreatedAt time.Time
}

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers and avoids SQLITE_BUSY between the API and the worker.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(timeFormat) }

const cardColumns = "id, code, state, message, attempts, created_at, updated_at, redeemed_at"

func scanCard(row interface{ Scan(...any) error }) (*GiftCard, error) {
	var c GiftCard
	var created, updated string
	var redeemed sql.NullString
	if err := row.Scan(&c.ID, &c.Code, &c.State, &c.Message, &c.Attempts, &created, &updated, &redeemed); err != nil {
		return nil, err
	}
	c.CreatedAt, _ = time.Parse(timeFormat, created)
	c.UpdatedAt, _ = time.Parse(timeFormat, updated)
	if redeemed.Valid {
		t, _ := time.Parse(timeFormat, redeemed.String)
		c.RedeemedAt = &t
	}
	return &c, nil
}

func addEvent(ctx context.Context, tx *sql.Tx, id, state, msg, ts string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO giftcard_events (giftcard_id, state, message, created_at) VALUES (?, ?, ?, ?)`,
		id, state, msg, ts)
	return err
}

// Create inserts a new gift card. If the code already exists, the existing card is
// returned with created=false.
func (s *Store) Create(ctx context.Context, code, state, msg string) (card *GiftCard, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	id, ts := newUUID(), now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO giftcards (id, code, state, message, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (code) DO NOTHING`,
		id, code, state, msg, ts, ts)
	if err != nil {
		tx.Rollback()
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		tx.Rollback()
		card, err := s.GetByCode(ctx, code)
		return card, false, err
	}
	if err := addEvent(ctx, tx, id, state, msg, ts); err != nil {
		tx.Rollback()
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	card, err = s.GetByID(ctx, id)
	return card, true, err
}

func (s *Store) GetByID(ctx context.Context, id string) (*GiftCard, error) {
	return scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM giftcards WHERE id = ?`, id))
}

func (s *Store) GetByCode(ctx context.Context, code string) (*GiftCard, error) {
	return scanCard(s.db.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM giftcards WHERE code = ?`, code))
}

func (s *Store) Events(ctx context.Context, id string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, message, created_at FROM giftcard_events WHERE giftcard_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&e.State, &e.Message, &ts); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(timeFormat, ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

// Transition moves a card to state `to` if it is currently in one of `from`.
// It reports whether the card was updated.
func (s *Store) Transition(ctx context.Context, id string, from []string, to, msg string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	ts := now()
	args := []any{to, msg, ts, to, ts, id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE giftcards
		 SET state = ?, message = ?, updated_at = ?,
		     redeemed_at = CASE WHEN ? = '`+StateRedeemed+`' THEN ? ELSE redeemed_at END
		 WHERE id = ? AND state IN (?`+strings.Repeat(", ?", len(from)-1)+`)`, args...)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := addEvent(ctx, tx, id, to, msg, ts); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ClaimNext atomically moves the oldest queued card to redeeming and returns it,
// or nil if the queue is empty.
func (s *Store) ClaimNext(ctx context.Context) (*GiftCard, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ts := now()
	card, err := scanCard(tx.QueryRowContext(ctx,
		`UPDATE giftcards SET state = ?, message = '', attempts = attempts + 1, updated_at = ?
		 WHERE id = (SELECT id FROM giftcards WHERE state = ? ORDER BY created_at LIMIT 1)
		 RETURNING `+cardColumns,
		StateRedeeming, ts, StateQueued))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, card.ID, StateRedeeming, "", ts); err != nil {
		return nil, err
	}
	return card, tx.Commit()
}

// RecoverInterrupted flags cards left in "redeeming" by a crash. They may or may not
// have been applied on Amazon, so they are not retried automatically.
func (s *Store) RecoverInterrupted(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM giftcards WHERE state = ?`, StateRedeeming)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.Transition(ctx, id, []string{StateRedeeming}, StateNeedsReview,
			"interrupted while redeeming; check the Amazon balance before retrying"); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
