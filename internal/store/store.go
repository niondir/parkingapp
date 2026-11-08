// Package store keeps users, their plates and the last known parking state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

type Plate struct {
	ChatID int64
	Plate  string
	Label  string
}

// User states for access control.
const (
	UserPending = "pending"
	UserAllowed = "allowed"
	UserBlocked = "blocked"
)

type User struct {
	ID        int64
	Username  string
	Name      string
	Status    string
	CreatedAt time.Time
}

// Session is a parking session seen by the monitor.
type Session struct {
	Plate        string
	Key          string
	Item         string // JSON of the parking item
	FirstSeen    time.Time
	LastSeen     time.Time
	Closed       bool
	ReminderSent bool
}

const schema = `
CREATE TABLE IF NOT EXISTS plates (
	chat_id    INTEGER NOT NULL,
	plate      TEXT    NOT NULL,
	label      TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (chat_id, plate)
);
CREATE TABLE IF NOT EXISTS sessions (
	plate         TEXT    NOT NULL,
	key           TEXT    NOT NULL,
	item          TEXT    NOT NULL,
	first_seen    INTEGER NOT NULL,
	last_seen     INTEGER NOT NULL,
	closed        INTEGER NOT NULL DEFAULT 0,
	reminder_sent INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (plate, key)
);
CREATE TABLE IF NOT EXISTS users (
	user_id    INTEGER PRIMARY KEY,
	username   TEXT    NOT NULL DEFAULT '',
	name       TEXT    NOT NULL DEFAULT '',
	status     TEXT    NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS plate_info (
	plate    TEXT PRIMARY KEY,
	quota_id INTEGER NOT NULL,
	address  TEXT NOT NULL DEFAULT ''
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// AddPlate adds or relabels a plate. It returns false if the plate already existed.
func (s *Store) AddPlate(ctx context.Context, chatID int64, plate, label string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO plates (chat_id, plate, label, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (chat_id, plate) DO NOTHING`, chatID, plate, label, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 && label != "" {
		_, err = s.db.ExecContext(ctx, `UPDATE plates SET label = ? WHERE chat_id = ? AND plate = ?`, label, chatID, plate)
	}
	return n > 0, err
}

func (s *Store) RemovePlate(ctx context.Context, chatID int64, plate string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plates WHERE chat_id = ? AND plate = ?`, chatID, plate)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountPlates(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plates WHERE chat_id = ?`, chatID).Scan(&n)
	return n, err
}

func (s *Store) Plates(ctx context.Context, chatID int64) ([]Plate, error) {
	return s.queryPlates(ctx, `SELECT chat_id, plate, label FROM plates WHERE chat_id = ? ORDER BY created_at`, chatID)
}

func (s *Store) HasPlate(ctx context.Context, chatID int64, plate string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plates WHERE chat_id = ? AND plate = ?`, chatID, plate).Scan(&n)
	return n > 0, err
}

// Subscribers returns all chats watching a plate.
func (s *Store) Subscribers(ctx context.Context, plate string) ([]Plate, error) {
	return s.queryPlates(ctx, `SELECT chat_id, plate, label FROM plates WHERE plate = ?`, plate)
}

// WatchedPlates returns every plate watched by at least one chat.
func (s *Store) WatchedPlates(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT plate FROM plates ORDER BY plate`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		res = append(res, p)
	}
	return res, rows.Err()
}

func (s *Store) queryPlates(ctx context.Context, q string, args ...any) ([]Plate, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Plate
	for rows.Next() {
		var p Plate
		if err := rows.Scan(&p.ChatID, &p.Plate, &p.Label); err != nil {
			return nil, err
		}
		res = append(res, p)
	}
	return res, rows.Err()
}

// OpenSessions returns the sessions of a plate that are not closed yet.
func (s *Store) OpenSessions(ctx context.Context, plate string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT plate, key, item, first_seen, last_seen, closed, reminder_sent
		 FROM sessions WHERE plate = ? AND closed = 0`, plate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Session
	for rows.Next() {
		var x Session
		var first, last int64
		if err := rows.Scan(&x.Plate, &x.Key, &x.Item, &first, &last, &x.Closed, &x.ReminderSent); err != nil {
			return nil, err
		}
		x.FirstSeen, x.LastSeen = time.Unix(first, 0), time.Unix(last, 0)
		res = append(res, x)
	}
	return res, rows.Err()
}

// UpsertSession stores a session as seen now. created is true for a new session.
func (s *Store) UpsertSession(ctx context.Context, plate, key, item string) (created bool, err error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (plate, key, item, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (plate, key) DO NOTHING`, plate, key, item, now, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET item = ?, last_seen = ?, closed = 0 WHERE plate = ? AND key = ?`, item, now, plate, key)
	return false, err
}

func (s *Store) CloseSession(ctx context.Context, plate, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET closed = 1 WHERE plate = ? AND key = ?`, plate, key)
	return err
}

func (s *Store) MarkReminderSent(ctx context.Context, plate, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET reminder_sent = 1 WHERE plate = ? AND key = ?`, plate, key)
	return err
}

// SetPlateInfo remembers the last parking area of a plate for tariff lookups.
func (s *Store) SetPlateInfo(ctx context.Context, plate string, quotaID int64, address string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO plate_info (plate, quota_id, address) VALUES (?, ?, ?)
		 ON CONFLICT (plate) DO UPDATE SET quota_id = excluded.quota_id, address = excluded.address`,
		plate, quotaID, address)
	return err
}

func (s *Store) PlateInfo(ctx context.Context, plate string) (quotaID int64, address string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT quota_id, address FROM plate_info WHERE plate = ?`, plate).Scan(&quotaID, &address)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// UserStatus returns the access state of a Telegram user or ErrNotFound.
func (s *Store) UserStatus(ctx context.Context, userID int64) (string, error) {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM users WHERE user_id = ?`, userID).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return st, err
}

// RequestAccess records a pending access request. created is false if the user is already known.
func (s *Store) RequestAccess(ctx context.Context, userID int64, username, name string) (created bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (user_id, username, name, status, created_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (user_id) DO NOTHING`, userID, username, name, UserPending, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetUserStatus sets the access state, creating the user if needed.
func (s *Store) SetUserStatus(ctx context.Context, userID int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (user_id, status, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET status = excluded.status`, userID, status, time.Now().Unix())
	return err
}

func (s *Store) GetUser(ctx context.Context, userID int64) (User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, username, name, status, created_at FROM users WHERE user_id = ?`, userID).
		Scan(&u.ID, &u.Username, &u.Name, &u.Status, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.CreatedAt = time.Unix(created, 0)
	return u, err
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, username, name, status, created_at FROM users
		 ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'allowed' THEN 1 ELSE 2 END, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []User
	for rows.Next() {
		var u User
		var created int64
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Status, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(created, 0)
		res = append(res, u)
	}
	return res, rows.Err()
}

// RemovePlates deletes all plates of a chat.
func (s *Store) RemovePlates(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM plates WHERE chat_id = ?`, chatID)
	return err
}
