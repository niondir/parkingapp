// Package store keeps users, their plates and the last known parking state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

type Plate struct {
	ChatID int64
	Plate  string
	Label  string
	PlateSettings
	CreatedAt time.Time
}

// PlateSettings are the notification settings of a chat for one plate.
type PlateSettings struct {
	Paused        bool          // no pushes at all
	Remind        bool          // reminder before the payment window closes
	PriceChange   bool          // push every price increase of an open session
	PriceLimit    float64       // push when the fee reaches this value, 0 = off
	DurationLimit time.Duration // push when parking takes longer, 0 = off
}

// ChatSettings are settings for all plates of a chat.
type ChatSettings struct {
	QuietFrom, QuietTo int // hours (local time); pushes in between are silent. Equal = off.
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
	ExitedAt     time.Time // zero while the car is (or was last seen) parked
	Outcome      string    // set when closed
	Missing      int       // consecutive checks in which the API didn't return it
}

// Session outcomes.
const (
	OutcomePaid  = "paid"  // left with a fee due, then paid
	OutcomeNoFee = "nofee" // left without a fee due: free or paid before leaving
)

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
CREATE TABLE IF NOT EXISTS chat_settings (
	chat_id    INTEGER PRIMARY KEY,
	quiet_from INTEGER NOT NULL DEFAULT 0,
	quiet_to   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

type column struct{ name, def string }

// columns added after the first release
var addedColumns = map[string][]column{
	"plates": plateColumns,
	"sessions": {
		{"exited_at", "INTEGER NOT NULL DEFAULT 0"},
		{"outcome", "TEXT NOT NULL DEFAULT ''"},
		{"missing", "INTEGER NOT NULL DEFAULT 0"}, // consecutive checks without the session
	},
}

var plateColumns = []column{
	{"paused", "INTEGER NOT NULL DEFAULT 0"},
	{"remind", "INTEGER NOT NULL DEFAULT 1"},
	{"price_change", "INTEGER NOT NULL DEFAULT 0"},
	{"price_limit", "REAL NOT NULL DEFAULT 0"},
	{"duration_limit", "INTEGER NOT NULL DEFAULT 0"}, // seconds
}

func migrate(db *sql.DB) error {
	for table, cols := range addedColumns {
		rows, err := db.Query(`SELECT name FROM pragma_table_info('` + table + `')`)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			have[n] = true
		}
		rows.Close()
		for _, c := range cols {
			if !have[c.name] {
				if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
					return err
				}
			}
		}
	}
	return nil
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

func (s *Store) SetLabel(ctx context.Context, chatID int64, plate, label string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plates SET label = ? WHERE chat_id = ? AND plate = ?`, label, chatID, plate)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
	return s.queryPlates(ctx, `WHERE chat_id = ? ORDER BY created_at`, chatID)
}

func (s *Store) HasPlate(ctx context.Context, chatID int64, plate string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plates WHERE chat_id = ? AND plate = ?`, chatID, plate).Scan(&n)
	return n > 0, err
}

// Subscribers returns all chats watching a plate.
func (s *Store) Subscribers(ctx context.Context, plate string) ([]Plate, error) {
	return s.queryPlates(ctx, `WHERE plate = ?`, plate)
}

// WatchedPlates returns every plate watched by at least one chat that isn't paused.
func (s *Store) WatchedPlates(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT plate FROM plates WHERE paused = 0 ORDER BY plate`)
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

// GetPlate returns one plate of a chat or ErrNotFound.
func (s *Store) GetPlate(ctx context.Context, chatID int64, plate string) (Plate, error) {
	res, err := s.queryPlates(ctx, `WHERE chat_id = ? AND plate = ?`, chatID, plate)
	if err != nil {
		return Plate{}, err
	}
	if len(res) == 0 {
		return Plate{}, ErrNotFound
	}
	return res[0], nil
}

func (s *Store) SetPlateSettings(ctx context.Context, chatID int64, plate string, ps PlateSettings) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE plates SET paused = ?, remind = ?, price_change = ?, price_limit = ?, duration_limit = ?
		 WHERE chat_id = ? AND plate = ?`,
		ps.Paused, ps.Remind, ps.PriceChange, ps.PriceLimit, int64(ps.DurationLimit/time.Second), chatID, plate)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) queryPlates(ctx context.Context, where string, args ...any) ([]Plate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, plate, label, paused, remind, price_change, price_limit, duration_limit, created_at
		 FROM plates `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Plate
	for rows.Next() {
		var p Plate
		var dur, created int64
		if err := rows.Scan(&p.ChatID, &p.Plate, &p.Label, &p.Paused, &p.Remind, &p.PriceChange,
			&p.PriceLimit, &dur, &created); err != nil {
			return nil, err
		}
		p.DurationLimit = time.Duration(dur) * time.Second
		p.CreatedAt = time.Unix(created, 0)
		res = append(res, p)
	}
	return res, rows.Err()
}

func (s *Store) GetChatSettings(ctx context.Context, chatID int64) (ChatSettings, error) {
	var cs ChatSettings
	err := s.db.QueryRowContext(ctx, `SELECT quiet_from, quiet_to FROM chat_settings WHERE chat_id = ?`, chatID).
		Scan(&cs.QuietFrom, &cs.QuietTo)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return cs, err
}

func (s *Store) SetChatSettings(ctx context.Context, chatID int64, cs ChatSettings) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_settings (chat_id, quiet_from, quiet_to) VALUES (?, ?, ?)
		 ON CONFLICT (chat_id) DO UPDATE SET quiet_from = excluded.quiet_from, quiet_to = excluded.quiet_to`,
		chatID, cs.QuietFrom, cs.QuietTo)
	return err
}

// OpenSessions returns the sessions of a plate that are not closed yet.
func (s *Store) OpenSessions(ctx context.Context, plate string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions s WHERE plate = ? AND closed = 0`, plate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, x)
	}
	return res, rows.Err()
}

// UpsertSession stores a session as seen now. created is true for a new
// session, reopened for one that was closed before.
func (s *Store) UpsertSession(ctx context.Context, plate, key, item string) (created, reopened bool, err error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (plate, key, item, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (plate, key) DO NOTHING`, plate, key, item, now, now)
	if err != nil {
		return false, false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, false, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT closed FROM sessions WHERE plate = ? AND key = ?`, plate, key).Scan(&reopened); err != nil {
		return false, false, err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET item = ?, last_seen = ?, closed = 0, missing = 0,
		 outcome = CASE WHEN closed = 1 THEN '' ELSE outcome END
		 WHERE plate = ? AND key = ?`, item, now, plate, key)
	return false, reopened, err
}

// MarkMissing counts a check in which the session was not returned.
func (s *Store) MarkMissing(ctx context.Context, plate, key string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`UPDATE sessions SET missing = missing + 1 WHERE plate = ? AND key = ? RETURNING missing`, plate, key).Scan(&n)
	return n, err
}

const sessionCols = `s.plate, s.key, s.item, s.first_seen, s.last_seen, s.closed, s.reminder_sent, s.exited_at, s.outcome, s.missing`

func scanSession(rows *sql.Rows) (Session, error) {
	var x Session
	var first, last, exited int64
	err := rows.Scan(&x.Plate, &x.Key, &x.Item, &first, &last, &x.Closed, &x.ReminderSent, &exited, &x.Outcome, &x.Missing)
	x.FirstSeen, x.LastSeen = time.Unix(first, 0), time.Unix(last, 0)
	if exited > 0 {
		x.ExitedAt = time.Unix(exited, 0)
	}
	return x, err
}

func (s *Store) CloseSession(ctx context.Context, plate, key, outcome string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET closed = 1, outcome = ? WHERE plate = ? AND key = ?`, outcome, plate, key)
	return err
}

// MarkExited records that the car left the car park.
func (s *Store) MarkExited(ctx context.Context, plate, key string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET exited_at = ? WHERE plate = ? AND key = ?`, t.Unix(), plate, key)
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

// Heartbeat records that the bot is running.
func (s *Store) Heartbeat(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('last_alive', ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(t.Unix(), 10))
	return err
}

// LastAlive returns the time of the last heartbeat or ErrNotFound.
func (s *Store) LastAlive(ctx context.Context) (time.Time, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_alive'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	return time.Unix(sec, 0), err
}

// Chats returns every chat that watches a plate or belongs to an approved user.
func (s *Store) Chats(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id FROM plates UNION SELECT user_id FROM users WHERE status = ?`, UserAllowed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		res = append(res, id)
	}
	return res, rows.Err()
}

// History returns the sessions of the chat's plates, newest first. Only
// sessions seen after the chat added the plate are included, so users don't
// see what happened before they watched a plate.
func (s *Store) History(ctx context.Context, chatID int64, since time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+`
		 FROM sessions s JOIN plates p ON p.plate = s.plate AND p.chat_id = ?
		 WHERE s.first_seen >= p.created_at AND s.first_seen >= ?
		 ORDER BY s.first_seen DESC`, chatID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, x)
	}
	return res, rows.Err()
}

// Stats are counters for admins.
type Stats struct {
	Users        map[string]int
	Chats        int
	Plates       int
	OpenSessions int
	Sessions30d  int
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Users: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM users GROUP BY status`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.Users[k] = n
	}
	rows.Close()
	err = s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(DISTINCT chat_id) FROM plates), (SELECT COUNT(*) FROM plates),
		        (SELECT COUNT(*) FROM sessions WHERE closed = 0), (SELECT COUNT(*) FROM sessions WHERE first_seen >= ?)`,
		time.Now().Add(-30*24*time.Hour).Unix()).Scan(&st.Chats, &st.Plates, &st.OpenSessions, &st.Sessions30d)
	return st, err
}
