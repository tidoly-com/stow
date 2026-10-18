// Package store is the library's record: who may come in, which photos exist, and what is left to do.
// It is one SQLite file. Every other package reaches the database through here, so that a different
// engine can be put behind the same calls later.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         TEXT PRIMARY KEY,
	sub        TEXT NOT NULL UNIQUE,   -- who home says this is
	email      TEXT NOT NULL,
	name       TEXT NOT NULL,
	is_admin   INTEGER NOT NULL,
	dir        TEXT NOT NULL UNIQUE,   -- folder under originals/, fixed when the user first comes in
	created_at TEXT NOT NULL,
	seq        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash BLOB PRIMARY KEY,       -- sha256 of the token; the token itself is never stored
	user_id    TEXT NOT NULL REFERENCES users(id),
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS used_tickets (
	jti TEXT PRIMARY KEY,
	exp INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS assets (
	id               TEXT PRIMARY KEY,
	owner_id         TEXT NOT NULL REFERENCES users(id),
	checksum         BLOB NOT NULL,    -- sha1 of the file, what the Immich app compares
	original_name    TEXT NOT NULL,
	path             TEXT NOT NULL,    -- relative to originals/, forward slashes
	size             INTEGER NOT NULL,
	type             TEXT NOT NULL,
	file_created_at  TEXT NOT NULL,
	file_modified_at TEXT NOT NULL,
	duration_ms      INTEGER NOT NULL,
	created_at       TEXT NOT NULL,
	seq              INTEGER NOT NULL,
	UNIQUE (owner_id, checksum)
);
CREATE INDEX IF NOT EXISTS assets_owner_seq ON assets(owner_id, seq);
CREATE TABLE IF NOT EXISTS asset_exif (  -- what the file says about itself; a row exists once it has been read
	asset_id      TEXT PRIMARY KEY REFERENCES assets(id),
	taken_at      TEXT,                -- the moment of capture, UTC
	local_time    TEXT,                -- the clock on the wall where it was taken, written as if UTC
	zone_seconds  INTEGER,             -- offset between the two, only if the file states it
	width         INTEGER, height INTEGER, orientation INTEGER,
	make          TEXT, model TEXT, lens TEXT,
	lat           REAL, lon REAL,
	f_number      REAL, exposure_time REAL, focal_length REAL, iso INTEGER, fps REAL
);
CREATE TABLE IF NOT EXISTS persons (     -- faces put together as one person; exists from the second matching face on
	id         TEXT PRIMARY KEY,
	owner_id   TEXT NOT NULL REFERENCES users(id),
	name       TEXT NOT NULL DEFAULT '',
	face_id    TEXT NOT NULL,          -- the face shown for this person
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	seq        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS faces (
	id        TEXT PRIMARY KEY,
	asset_id  TEXT NOT NULL REFERENCES assets(id),
	owner_id  TEXT NOT NULL REFERENCES users(id),
	person_id TEXT REFERENCES persons(id),
	x1 INTEGER, y1 INTEGER, x2 INTEGER, y2 INTEGER,  -- in pixels of the upright preview
	image_w INTEGER, image_h INTEGER,
	score     REAL,
	embedding BLOB NOT NULL,           -- float32, little-endian, length 1
	seq       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS faces_owner ON faces(owner_id, seq);
CREATE INDEX IF NOT EXISTS faces_asset ON faces(asset_id);
CREATE TABLE IF NOT EXISTS albums (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL REFERENCES users(id),
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	cover_id    TEXT,                  -- the photo chosen to stand for the album; the newest one if not chosen
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	seq         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS album_assets (
	album_id TEXT NOT NULL REFERENCES albums(id),
	asset_id TEXT NOT NULL REFERENCES assets(id),
	seq      INTEGER NOT NULL,
	PRIMARY KEY (album_id, asset_id)
);
CREATE TABLE IF NOT EXISTS deletions (   -- what is gone, so that devices which knew of it can forget it
	seq      INTEGER PRIMARY KEY,
	owner_id TEXT NOT NULL,
	kind     TEXT NOT NULL,          -- album, album_asset, asset, face, person
	a        TEXT NOT NULL,
	b        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS jobs (
	id       INTEGER PRIMARY KEY,
	asset_id TEXT NOT NULL REFERENCES assets(id),
	kind     TEXT NOT NULL,
	state    TEXT NOT NULL,            -- queued, running, done, failed
	attempts INTEGER NOT NULL DEFAULT 0,
	error    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state, id);
CREATE INDEX IF NOT EXISTS jobs_asset ON jobs(asset_id, kind); -- finding the photos a kind of job was never done for, at every start
CREATE TABLE IF NOT EXISTS sync_acks (
	token_hash BLOB NOT NULL,
	type       TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	PRIMARY KEY (token_hash, type)
);
CREATE TABLE IF NOT EXISTS locations (   -- folders, besides the data folder, that hold photos (#14)
	id         TEXT PRIMARY KEY,
	path       TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (name TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS skipped (     -- files in a read-only folder that their owner deleted from the library: not to be read in again
	location TEXT NOT NULL,
	path     TEXT NOT NULL,
	PRIMARY KEY (location, path)
);
CREATE TABLE IF NOT EXISTS coupons (  -- pictures the coupon job found to be coupons; once found, never put in the album again
	asset_id TEXT PRIMARY KEY REFERENCES assets(id),
	code     TEXT NOT NULL,   -- the barcode's text
	expires  TEXT NOT NULL    -- the last day it can be used, 2026-12-31
);
CREATE TABLE IF NOT EXISTS memories (  -- "N년 전 이달": for each person, one a day, of this month's photos from one earlier year
	id         TEXT PRIMARY KEY,
	owner_id   TEXT NOT NULL REFERENCES users(id),
	day        TEXT NOT NULL,     -- the day it is shown, 2026-10-14
	year       INTEGER NOT NULL,  -- the year its photos are from
	created_at TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	UNIQUE (owner_id, day)
);
CREATE TABLE IF NOT EXISTS memory_assets (
	memory_id TEXT NOT NULL REFERENCES memories(id),
	asset_id  TEXT NOT NULL REFERENCES assets(id),
	seq       INTEGER NOT NULL,
	PRIMARY KEY (memory_id, asset_id)
);
CREATE TABLE IF NOT EXISTS shared_links (  -- an address for someone without an account: one album, or some photos
	id             TEXT PRIMARY KEY,
	key            TEXT NOT NULL UNIQUE,     -- the secret in the address
	owner_id       TEXT NOT NULL REFERENCES users(id),
	album_id       TEXT REFERENCES albums(id), -- the album shown; NULL for photos chosen one by one
	description    TEXT NOT NULL DEFAULT '',
	password       TEXT NOT NULL DEFAULT '', -- a hash; '' for none
	expires_at     TEXT,                     -- NULL for never
	allow_download INTEGER NOT NULL DEFAULT 1,
	show_metadata  INTEGER NOT NULL DEFAULT 1,
	created_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS shared_link_assets (
	link_id  TEXT NOT NULL REFERENCES shared_links(id),
	asset_id TEXT NOT NULL REFERENCES assets(id),
	PRIMARY KEY (link_id, asset_id)
);
CREATE TABLE IF NOT EXISTS counters (name TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT OR IGNORE INTO counters VALUES ('seq', 0);
`

var added = []string{
	`asset_exif ADD COLUMN city TEXT`, // the place the coordinates fall in, named from the data inside the program
	`asset_exif ADD COLUMN state TEXT`,
	`asset_exif ADD COLUMN country TEXT`,
	`persons ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`,    // put away by its owner: a doll, a poster, a stranger
	`assets ADD COLUMN shared INTEGER NOT NULL DEFAULT 0`,     // shown to everyone on this server; still its owner's (#13)
	`assets ADD COLUMN location TEXT NOT NULL DEFAULT ''`,     // which storage location holds the file; '' is the data folder (#14)
	`users ADD COLUMN location TEXT NOT NULL DEFAULT ''`,      // where this person's new photos go; '' is wherever everyone's go
	`locations ADD COLUMN kind TEXT NOT NULL DEFAULT 'store'`, // 'store': we put photos there. 'folder': someone's own folder, only read (#15)
	`locations ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,  // whose library a read-only folder's photos join
	`locations ADD COLUMN paused INTEGER NOT NULL DEFAULT 0`,  // a read-only folder switched off for now: recorded, not shown
	`assets ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`,     // in a folder that is switched off: kept, with its small pictures, and shown nowhere
	`users ADD COLUMN password TEXT NOT NULL DEFAULT ''`,      // for a server that keeps its own accounts: the password, hashed. '' with tidoly's login
	`assets ADD COLUMN live_of TEXT NOT NULL DEFAULT ''`,      // on the moving half of a live photo: the photo it belongs to. It is listed nowhere by itself
	`assets ADD COLUMN deleted_at TEXT`,                       // in the trash since then; the files are still on disk
	`assets ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0`,
	`sessions ADD COLUMN expires_at TEXT`,                 // for sessions lent to another device for a while, such as a TV being cast to
	`albums ADD COLUMN auto TEXT NOT NULL DEFAULT ''`,     // an album the server fills by itself: 'coupons'. '' for one a person made
	`sessions ADD COLUMN via TEXT NOT NULL DEFAULT ''`,    // how it was opened: 'home' (this server's password) or 'tidoly' (a ticket). '' before this was kept
	`users ADD COLUMN home_at TEXT`,                       // first came in with this server's password
	`users ADD COLUMN tidoly_at TEXT`,                     // first came in with tidoly's login — from outside, through the tunnel
	`users ADD COLUMN blocked INTEGER NOT NULL DEFAULT 0`, // the owner shut tidoly's login for this person; at home the password still works
	`memories ADD COLUMN gone INTEGER NOT NULL DEFAULT 0`, // its day is past: taken off the devices, kept here for which year it showed
}

// ErrDuplicate means the owner already has a file with the same checksum.
var ErrDuplicate = errors.New("duplicate asset")

const timeLayout = "2006-01-02T15:04:05.000Z"

// Timestamp formats t the way the Immich API writes dates.
func Timestamp(t time.Time) string { return t.UTC().Format(timeLayout) }

type Store struct{ db *sql.DB }

type User struct {
	ID, Sub, Email, Name, Dir, CreatedAt string
	Location                             string // where this person's new photos go; "" = the default
	IsAdmin                              bool
	Seq                                  int64
	HomeAt, TidolyAt                     string // when they first came in, by each way; "" if never
	Blocked                              bool   // tidoly's login is shut for them
}

type Asset struct {
	ID, OwnerID, OriginalName, Path, Type string
	Location                              string // the storage location holding the file; "" = the data folder
	LiveOf                                string // on the moving half of a live photo: the photo's id
	Live                                  string // on a live photo: the id of its moving half
	Checksum                              []byte
	Size, DurationMs, Seq                 int64
	FileCreatedAt, FileModifiedAt         string
	CreatedAt                             string
	DeletedAt                             string // in the trash since then; "" if not
	Favorite                              bool
	Shared                                bool  // everyone on this server may see it
	Meta                                  *Meta // nil until the file has been read
}

// Meta is what a file says about itself. Zero values and nil pointers mean it does not say.
type Meta struct {
	TakenAt, LocalTime                 string
	ZoneSeconds                        *int
	Width, Height, Orientation, ISO    int
	Make, Model, Lens                  string
	Lat, Lon                           *float64
	FNumber, ExposureTime, FocalLength float64
	FPS                                float64
	City, State, Country               string // filled in by the place job, after the coordinates are known
}

type Job struct {
	ID       int64
	AssetID  string
	Kind     string
	Attempts int
}

func Open(path string) (*Store, error) {
	// _txlock=immediate: a transaction that will write takes the write lock up front instead of failing halfway
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	for _, column := range added { // columns newer than the tables a data folder may already hold
		if _, err := db.Exec(`ALTER TABLE ` + column); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	// an index on a column that was added above cannot be made before it. It holds only the moving halves of
	// live photos; a query uses it only when it says live_of != '' itself — without that, finding a photo's
	// moving half read every photo, for every photo listed (a month of 11,000 took 50 s, 2026-10-18)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS assets_live ON assets(live_of) WHERE live_of != ''`); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NewID returns a random UUID (version 4), the id shape the Immich app expects.
func NewID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nextSeq(ctx context.Context, tx *sql.Tx) (seq int64, err error) {
	err = tx.QueryRowContext(ctx, `UPDATE counters SET value = value + 1 WHERE name = 'seq' RETURNING value`).Scan(&seq)
	return
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// ── users and sessions ──────────────────────────────────────────────────────────────────────────────────────

// UpsertUser records what a ticket says about a person. The folder name is chosen once and kept.
func (s *Store) UpsertUser(ctx context.Context, sub, email, name string, admin bool) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return User{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, name = ?, is_admin = ?, seq = ? WHERE sub = ?`, email, name, admin, seq, sub)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		dir, err := freeDir(ctx, tx, email)
		if err != nil {
			return User{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, sub, email, name, is_admin, dir, created_at, seq) VALUES (?,?,?,?,?,?,?,?)`,
			NewID(), sub, email, name, admin, dir, Timestamp(time.Now()), seq); err != nil {
			return User{}, err
		}
	}
	u, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE sub = ?`, sub))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// freeDir turns an email into a folder name nobody else has.
func freeDir(ctx context.Context, tx *sql.Tx, email string) (string, error) {
	base := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' || r == '@' {
			return r
		}
		return '_'
	}, strings.ToLower(email))
	base = strings.Trim(base, ".")
	if base == "" {
		base = "user"
	}
	for i := 1; ; i++ {
		dir := base
		if i > 1 {
			dir = fmt.Sprintf("%s-%d", base, i)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE dir = ?`, dir).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return dir, nil
		}
	}
}

const (
	userColumns = `u.id, u.sub, u.email, u.name, u.is_admin, u.dir, u.created_at, u.seq, u.location, coalesce(u.home_at, ''), coalesce(u.tidoly_at, ''), u.blocked`
	userSelect  = `SELECT ` + userColumns + ` FROM users u`
)

func scanUser(row scanner) (u User, err error) {
	err = row.Scan(&u.ID, &u.Sub, &u.Email, &u.Name, &u.IsAdmin, &u.Dir, &u.CreatedAt, &u.Seq, &u.Location, &u.HomeAt, &u.TidolyAt, &u.Blocked)
	return
}

// Ways a session is opened.
const (
	ViaHome   = "home"   // this server's address and password, at home
	ViaTidoly = "tidoly" // a ticket from tidoly-home
)

// CreateSession opens a session, and notes the first time the person came in this way.
func (s *Store) CreateSession(ctx context.Context, token, userID, via string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Timestamp(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, via) VALUES (?,?,?,?)`, hashToken(token), userID, now, via); err != nil {
		return err
	}
	column := map[string]string{ViaHome: "home_at", ViaTidoly: "tidoly_at"}[via]
	if column != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET `+column+` = ? WHERE id = ? AND `+column+` IS NULL`, now, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateTimedSession opens a session that ends by itself, and forgets the ones that already have.
func (s *Store) CreateTimedSession(ctx context.Context, token, userID string, until time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at IS NOT NULL AND expires_at < ?`, Timestamp(time.Now())); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?,?,?,?)`,
		hashToken(token), userID, Timestamp(time.Now()), Timestamp(until))
	return err
}

// SessionUser returns the user a token belongs to; sql.ErrNoRows if the token opens nothing.
func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+`
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ? AND (s.expires_at IS NULL OR s.expires_at > ?)`,
		hashToken(token), Timestamp(time.Now())))
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	h := hashToken(token)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sync_acks WHERE token_hash = ?`, h); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, h)
	return err
}

// DropTidolySessionsExcept closes the sessions opened with tidoly's login by everyone whose address is not
// in members, and reports how many closed. Sessions opened at home with this server's password stay: a
// person home no longer lists for this server may still have an account here.
func (s *Store) DropTidolySessionsExcept(ctx context.Context, members []string) (int64, error) {
	marks := strings.TrimSuffix(strings.Repeat("lower(?),", len(members)), ",")
	args := []any{ViaTidoly}
	for _, m := range members {
		args = append(args, m)
	}
	where := `via = ? AND user_id IN (SELECT id FROM users WHERE lower(email) NOT IN (` + marks + `))`
	return s.dropSessions(ctx, where, args)
}

func (s *Store) dropSessions(ctx context.Context, where string, args []any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sync_acks WHERE token_hash IN (SELECT token_hash FROM sessions WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// Block shuts tidoly's login for a person, closing the sessions they opened with it, or opens it again.
func (s *Store) Block(ctx context.Context, userID string, blocked bool) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET blocked = ? WHERE id = ?`, blocked, userID); err != nil {
		return err
	}
	if !blocked {
		return nil
	}
	_, err := s.dropSessions(ctx, `via = ? AND user_id = ?`, []any{ViaTidoly, userID})
	return err
}

// SpendTicket records a ticket id; false means it was spent before. Expired ids are forgotten on the way.
func (s *Store) SpendTicket(ctx context.Context, jti string, exp int64) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM used_tickets WHERE exp < ?`, time.Now().Unix()-3600); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO used_tickets (jti, exp) VALUES (?,?)`, jti, exp)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ── assets ──────────────────────────────────────────────────────────────────────────────────────────────────

func (s *Store) AssetByChecksum(ctx context.Context, ownerID string, checksum []byte) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, assetSelect+` WHERE a.owner_id = ? AND a.checksum = ?`, ownerID, checksum))
}

func (s *Store) Asset(ctx context.Context, ownerID, id string) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, assetSelect+` WHERE a.owner_id = ? AND a.id = ?`, ownerID, id))
}

// AssetByID is for background work, which acts for no particular person.
// AssetFor finds a photo the viewer may look at: their own, or one that its owner shares and has not
// put in the trash.
func (s *Store) AssetFor(ctx context.Context, viewerID, id string) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, assetSelect+` WHERE a.id = ? AND a.hidden = 0 AND (a.owner_id = ? OR (a.shared = 1 AND a.deleted_at IS NULL))`, id, viewerID))
}

func (s *Store) AssetByID(ctx context.Context, id string) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, assetSelect+` WHERE a.id = ?`, id))
}

// SetMeta records what was read from the file. The asset counts as changed, so every device hears of it
// at its next sync. durationMs replaces the phone's figure only when the file states one.
func (s *Store) SetMeta(ctx context.Context, assetID string, m Meta, durationMs int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	null := func(v any, empty bool) any {
		if empty {
			return nil
		}
		return v
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO asset_exif (asset_id, taken_at, local_time, zone_seconds, width, height, orientation,
		make, model, lens, lat, lon, f_number, exposure_time, focal_length, iso, fps) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		assetID, null(m.TakenAt, m.TakenAt == ""), null(m.LocalTime, m.LocalTime == ""), m.ZoneSeconds,
		null(m.Width, m.Width == 0), null(m.Height, m.Height == 0), null(m.Orientation, m.Orientation == 0),
		null(m.Make, m.Make == ""), null(m.Model, m.Model == ""), null(m.Lens, m.Lens == ""), m.Lat, m.Lon,
		null(m.FNumber, m.FNumber == 0), null(m.ExposureTime, m.ExposureTime == 0), null(m.FocalLength, m.FocalLength == 0),
		null(m.ISO, m.ISO == 0), null(m.FPS, m.FPS == 0)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET seq = ?, duration_ms = CASE WHEN ? > 0 THEN ? ELSE duration_ms END WHERE id = ?`,
		seq, durationMs, durationMs, assetID); err != nil {
		return err
	}
	return tx.Commit()
}

// PathTaken reports whether a stored file already sits at this path.
func (s *Store) PathTaken(ctx context.Context, location, path string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM assets WHERE location = ? AND path = ?`, location, path).Scan(&n)
	return n > 0, err
}

// AddAsset records a stored file and queues its work in one step, so a photo never exists without its jobs.
func (s *Store) AddAsset(ctx context.Context, a *Asset, jobKinds []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a.Seq, err = nextSeq(ctx, tx); err != nil {
		return err
	}
	a.CreatedAt = Timestamp(time.Now())
	_, err = tx.ExecContext(ctx, `INSERT INTO assets (id, owner_id, checksum, original_name, location, path, size, type, file_created_at, file_modified_at, duration_ms, created_at, seq)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.OwnerID, a.Checksum, a.OriginalName, a.Location, a.Path, a.Size, a.Type, a.FileCreatedAt, a.FileModifiedAt, a.DurationMs, a.CreatedAt, a.Seq)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: assets.owner_id, assets.checksum") {
			return ErrDuplicate
		}
		return err
	}
	for _, kind := range jobKinds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (asset_id, kind, state) VALUES (?,?,'queued')`, a.ID, kind); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const assetSelect = `SELECT a.id, a.owner_id, a.checksum, a.original_name, a.path, a.size, a.type, a.file_created_at, a.file_modified_at,
	a.duration_ms, a.created_at, a.seq, coalesce(a.deleted_at, ''), a.favorite, a.shared, a.location, a.live_of,
	coalesce((SELECT v.id FROM assets v WHERE v.live_of = a.id AND v.live_of != '' AND a.live_of = '' LIMIT 1), ''), e.asset_id, e.taken_at, e.local_time, e.zone_seconds, e.width, e.height, e.orientation,
	e.make, e.model, e.lens, e.lat, e.lon, e.f_number, e.exposure_time, e.focal_length, e.iso, e.fps, e.city, e.state, e.country
	FROM assets a LEFT JOIN asset_exif e ON e.asset_id = a.id`

type scanner interface{ Scan(...any) error }

func scanAsset(row scanner) (a Asset, err error) {
	var (
		read                             sql.NullString
		taken, local, make_, model, lens sql.NullString
		city, state, country             sql.NullString
		zone, width, height, orient, iso sql.NullInt64
		lat, lon, fnum, exp, focal, fps  sql.NullFloat64
	)
	err = row.Scan(&a.ID, &a.OwnerID, &a.Checksum, &a.OriginalName, &a.Path, &a.Size, &a.Type, &a.FileCreatedAt, &a.FileModifiedAt,
		&a.DurationMs, &a.CreatedAt, &a.Seq, &a.DeletedAt, &a.Favorite, &a.Shared, &a.Location, &a.LiveOf, &a.Live, &read, &taken, &local, &zone, &width, &height, &orient,
		&make_, &model, &lens, &lat, &lon, &fnum, &exp, &focal, &iso, &fps, &city, &state, &country)
	if err != nil || !read.Valid {
		return
	}
	a.Meta = &Meta{TakenAt: taken.String, LocalTime: local.String, Width: int(width.Int64), Height: int(height.Int64),
		Orientation: int(orient.Int64), ISO: int(iso.Int64), Make: make_.String, Model: model.String, Lens: lens.String,
		FNumber: fnum.Float64, ExposureTime: exp.Float64, FocalLength: focal.Float64, FPS: fps.Float64,
		City: city.String, State: state.String, Country: country.String}
	if zone.Valid {
		z := int(zone.Int64)
		a.Meta.ZoneSeconds = &z
	}
	if lat.Valid && lon.Valid {
		a.Meta.Lat, a.Meta.Lon = &lat.Float64, &lon.Float64
	}
	return
}

// SetPlace names where an asset was taken. The asset counts as changed.
func (s *Store) SetPlace(ctx context.Context, assetID, city, state, country string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE asset_exif SET city = ?, state = ?, country = ? WHERE asset_id = ?`, city, state, country, assetID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET seq = ? WHERE id = ?`, seq, assetID); err != nil {
		return err
	}
	return tx.Commit()
}

// LocationCounts tells how many of a person's camera photos say where they were taken.
// A camera photo is one that names the camera; screenshots and downloads do not.
func (s *Store) LocationCounts(ctx context.Context, ownerID string) (camera, located int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), count(e.lat) FROM asset_exif e JOIN assets a ON a.id = e.asset_id
		WHERE a.owner_id = ? AND e.model IS NOT NULL`, ownerID).Scan(&camera, &located)
	return
}

// AssetsSince walks the owner's assets changed after seq, oldest change first.
func (s *Store) AssetsSince(ctx context.Context, ownerID string, seq int64, each func(Asset) error) error {
	rows, err := s.db.QueryContext(ctx, assetSelect+` WHERE a.owner_id = ? AND a.seq > ? AND a.hidden = 0 ORDER BY a.seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return err
		}
		if err := each(a); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ── sync checkpoints ────────────────────────────────────────────────────────────────────────────────────────

// Acks returns how far this session has confirmed each kind of record.
func (s *Store) Acks(ctx context.Context, token string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, seq FROM sync_acks WHERE token_hash = ?`, hashToken(token))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	acks := map[string]int64{}
	for rows.Next() {
		var kind string
		var seq int64
		if err := rows.Scan(&kind, &seq); err != nil {
			return nil, err
		}
		acks[kind] = seq
	}
	return acks, rows.Err()
}

func (s *Store) SetAck(ctx context.Context, token, kind string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_acks (token_hash, type, seq) VALUES (?,?,?)
		ON CONFLICT (token_hash, type) DO UPDATE SET seq = max(seq, excluded.seq)`, hashToken(token), kind, seq)
	return err
}

func (s *Store) ResetAcks(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sync_acks WHERE token_hash = ?`, hashToken(token))
	return err
}

// ── jobs ────────────────────────────────────────────────────────────────────────────────────────────────────

// AddJob queues one more piece of work for an asset.
func (s *Store) AddJob(ctx context.Context, assetID, kind string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (asset_id, kind, state) VALUES (?,?,'queued')`, assetID, kind)
	return err
}

// UndatedEdits lists iPhone edited copies (IMG_E…) that were read without a capture time and are not
// waiting to be read: read again, they take the original's.
func (s *Store) UndatedEdits(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id FROM assets a JOIN asset_exif e ON e.asset_id = a.id
		WHERE a.original_name LIKE 'IMG\_E%' ESCAPE '\' AND e.taken_at IS NULL AND a.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.asset_id = a.id AND j.kind = 'metadata' AND j.state IN ('queued', 'running'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AssetsWithoutJob lists the assets that have no job of this kind waiting or running — or, with ever
// set, that never had one at all.
func (s *Store) AssetsWithoutJob(ctx context.Context, kind string, ever bool) ([]string, error) {
	states := ` AND j.state IN ('queued', 'running')`
	if ever {
		states = ``
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id FROM assets a WHERE NOT EXISTS
		(SELECT 1 FROM jobs j WHERE j.asset_id = a.id AND j.kind = ?`+states+`)`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QueueMissingPlaces queues the place job for assets that have coordinates, no place name, and no such
// job waiting — photos read by a version of the program that did not name places yet.
func (s *Store) QueueMissingPlaces(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs (asset_id, kind, state)
		SELECT e.asset_id, 'place', 'queued' FROM asset_exif e
		WHERE e.lat IS NOT NULL AND e.city IS NULL
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.asset_id = e.asset_id AND j.kind = 'place')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequeueRunning puts back the jobs a previous run of the program left half done.
func (s *Store) RequeueRunning(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = 'queued' WHERE state = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClaimJob hands out the oldest queued job of one of the given kinds; sql.ErrNoRows when there is none.
func (s *Store) ClaimJob(ctx context.Context, kinds []string) (j Job, err error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	err = s.db.QueryRowContext(ctx, `UPDATE jobs SET state = 'running', attempts = attempts + 1
		WHERE id = (SELECT id FROM jobs WHERE state = 'queued' AND kind IN (`+marks+`) ORDER BY id LIMIT 1)
		RETURNING id, asset_id, kind, attempts`, args...).Scan(&j.ID, &j.AssetID, &j.Kind, &j.Attempts)
	return
}

// FinishJob closes a job. A failed job goes back in the queue until it has been tried maxAttempts times.
func (s *Store) FinishJob(ctx context.Context, j Job, jobErr error, maxAttempts int) error {
	state, msg := "done", ""
	if jobErr != nil {
		state, msg = "queued", jobErr.Error()
		if j.Attempts >= maxAttempts {
			state = "failed"
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, error = ? WHERE id = ?`, state, msg, j.ID)
	return err
}

// JobCounts returns how many jobs are in each state.
func (s *Store) JobCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}

// ── faces and people ────────────────────────────────────────────────────────────────────────────────────────

type Face struct {
	ID, AssetID, OwnerID, PersonID string
	X1, Y1, X2, Y2, ImageW, ImageH int
	Score                          float64
	Embedding                      []float32
	Seq                            int64
}

type Person struct {
	ID, OwnerID, Name, FaceID, CreatedAt, UpdatedAt string
	Hidden                                          bool
	Seq                                             int64
}

func packFloats(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func unpackFloats(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// HasFaces reports whether an asset has been looked at for faces and some were found.
func (s *Store) HasFaces(ctx context.Context, assetID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM faces WHERE asset_id = ?`, assetID).Scan(&n)
	return n > 0, err
}

// FaceDescriptions returns every face of one person's library with its description, for comparing a new
// face against. Only id, person and description are filled in.
func (s *Store) FaceDescriptions(ctx context.Context, ownerID string) ([]Face, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, coalesce(person_id, ''), embedding FROM faces WHERE owner_id = ?`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Face
	for rows.Next() {
		var f Face
		var raw []byte
		if err := rows.Scan(&f.ID, &f.PersonID, &raw); err != nil {
			return nil, err
		}
		f.Embedding = unpackFloats(raw)
		out = append(out, f)
	}
	return out, rows.Err()
}

// AddFace records a face. If likeFaceID names an earlier face it resembles, the two are one person: the
// earlier face's person if it has one, otherwise a new person made of the two. It returns the id of a
// person created by this call, or "".
func (s *Store) AddFace(ctx context.Context, f *Face, likeFaceID string) (created string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return "", err
	}
	now := Timestamp(time.Now())
	f.ID, f.Seq, f.PersonID = NewID(), seq, ""
	if likeFaceID != "" {
		var person sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT person_id FROM faces WHERE id = ?`, likeFaceID).Scan(&person); err != nil {
			return "", err
		}
		f.PersonID = person.String
		if !person.Valid {
			f.PersonID, created = NewID(), ""
			created = f.PersonID
			if _, err := tx.ExecContext(ctx, `INSERT INTO persons (id, owner_id, face_id, created_at, updated_at, seq) VALUES (?,?,?,?,?,?)`,
				f.PersonID, f.OwnerID, likeFaceID, now, now, seq); err != nil {
				return "", err
			}
			// the earlier face now belongs to someone: it counts as changed too
			if _, err := tx.ExecContext(ctx, `UPDATE faces SET person_id = ?, seq = ? WHERE id = ?`, f.PersonID, seq, likeFaceID); err != nil {
				return "", err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE persons SET updated_at = ?, seq = ? WHERE id = ?`, now, seq, f.PersonID); err != nil {
			return "", err
		}
	}
	var person any
	if f.PersonID != "" {
		person = f.PersonID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO faces (id, asset_id, owner_id, person_id, x1, y1, x2, y2, image_w, image_h, score, embedding, seq)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.ID, f.AssetID, f.OwnerID, person, f.X1, f.Y1, f.X2, f.Y2, f.ImageW, f.ImageH, f.Score, packFloats(f.Embedding), seq); err != nil {
		return "", err
	}
	return created, tx.Commit()
}

// FaceByID returns one face with where it is, without its description.
func (s *Store) FaceByID(ctx context.Context, id string) (f Face, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, asset_id, owner_id, coalesce(person_id, ''), x1, y1, x2, y2, image_w, image_h, score, seq FROM faces WHERE id = ?`, id).
		Scan(&f.ID, &f.AssetID, &f.OwnerID, &f.PersonID, &f.X1, &f.Y1, &f.X2, &f.Y2, &f.ImageW, &f.ImageH, &f.Score, &f.Seq)
	return
}

func (s *Store) Person(ctx context.Context, ownerID, id string) (p Person, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, owner_id, name, face_id, created_at, updated_at, hidden, seq FROM persons WHERE owner_id = ? AND id = ?`, ownerID, id).
		Scan(&p.ID, &p.OwnerID, &p.Name, &p.FaceID, &p.CreatedAt, &p.UpdatedAt, &p.Hidden, &p.Seq)
	return
}

// HidePerson puts a person away or brings them back. Faces that resemble a hidden person still join
// that person, so what was put away stays away. sql.ErrNoRows if the person is not the owner's.
func (s *Store) HidePerson(ctx context.Context, ownerID, id string, hidden bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE persons SET hidden = ?, updated_at = ?, seq = ? WHERE owner_id = ? AND id = ?`,
		hidden, Timestamp(time.Now()), seq, ownerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// NamePerson gives a person a name, or takes it away with "". sql.ErrNoRows if the person is not the owner's.
func (s *Store) NamePerson(ctx context.Context, ownerID, id, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE persons SET name = ?, updated_at = ?, seq = ? WHERE owner_id = ? AND id = ?`,
		name, Timestamp(time.Now()), seq, ownerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// LoneFaces counts the owner's faces that belong to nobody yet: seen once, waiting for a second sighting.
func (s *Store) LoneFaces(ctx context.Context, ownerID string) (n int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM faces WHERE owner_id = ? AND person_id IS NULL`, ownerID).Scan(&n)
	return
}

// PeopleSince walks the owner's people changed after seq.
func (s *Store) PeopleSince(ctx context.Context, ownerID string, seq int64, each func(Person) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner_id, name, face_id, created_at, updated_at, hidden, seq FROM persons WHERE owner_id = ? AND seq > ? ORDER BY seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.OwnerID, &p.Name, &p.FaceID, &p.CreatedAt, &p.UpdatedAt, &p.Hidden, &p.Seq); err != nil {
			return err
		}
		if err := each(p); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FacesSince walks the owner's faces changed after seq, without their descriptions.
func (s *Store) FacesSince(ctx context.Context, ownerID string, seq int64, each func(Face) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, asset_id, owner_id, coalesce(person_id, ''), x1, y1, x2, y2, image_w, image_h, score, seq
		FROM faces WHERE owner_id = ? AND seq > ? ORDER BY seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f Face
		if err := rows.Scan(&f.ID, &f.AssetID, &f.OwnerID, &f.PersonID, &f.X1, &f.Y1, &f.X2, &f.Y2, &f.ImageW, &f.ImageH, &f.Score, &f.Seq); err != nil {
			return err
		}
		if err := each(f); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ── what the web pages ask ──────────────────────────────────────────────────────────────────────────────────

// when is the expression the pages order and group photos by: the wall-clock time of capture once the
// file has been read, the phone's time until then.
const when = `COALESCE(e.local_time, a.file_created_at)`

type Count struct {
	Key   string
	Count int
	Cover string // an asset to stand for the group
}

// Which photos a list is of. The zero value of Show is everything the viewer may see: their own and
// what the others share.
const (
	ShowMine      = "mine"      // only the viewer's own
	ShowShared    = "shared"    // what anyone shares, the viewer included; By narrows it to one person
	ShowFavorites = "favorites" // the viewer's own favorites
	ShowVideos    = "videos"    // the videos of all the viewer can see: their own and what others share
)

func scope(viewerID, show, by, at string) (string, []any) {
	if at != "" && show != ShowShared { // one storage location: only the viewer's own photos lie "somewhere" for them
		if at == HomeLocation {
			at = ""
		}
		where, args := `a.owner_id = ? AND a.location = ?`, []any{viewerID, at}
		if show == ShowFavorites {
			where += ` AND a.favorite = 1`
		}
		return where, args
	}
	switch show {
	case ShowMine:
		return `a.owner_id = ?`, []any{viewerID}
	case ShowFavorites:
		return `a.owner_id = ? AND a.favorite = 1`, []any{viewerID}
	case ShowVideos:
		return `(a.owner_id = ? OR a.shared = 1) AND a.type = 'VIDEO'`, []any{viewerID}
	case ShowShared:
		if by != "" {
			return `a.shared = 1 AND a.owner_id = ?`, []any{by}
		}
		return `a.shared = 1`, nil
	}
	return `(a.owner_id = ? OR a.shared = 1)`, []any{viewerID}
}

// Narrow keeps a list to the viewer's own photos from one place ("country", "country|state" or
// "country|state|city"), of one person, or in one album.
type Narrow struct{ Place, Person, Album string }

func (n Narrow) none() bool { return n == Narrow{} }

// Months lists the months that have photos of that kind, newest first, with how many. at narrows it to
// the viewer's own photos in one storage location (HomeLocation for the data folder); n, when set, to
// the viewer's own photos of a place, a person or an album, whatever show, by and at say.
func (s *Store) Months(ctx context.Context, viewerID, show, by, at string, n Narrow) ([]Count, error) {
	where, args := scope(viewerID, show, by, at)
	if !n.none() {
		where, args = narrowScope(viewerID, n)
	}
	return s.counts(ctx, `SELECT substr(`+when+`, 1, 7) AS k, count(*), '' FROM assets a LEFT JOIN asset_exif e ON e.asset_id = a.id
		WHERE `+where+` AND a.deleted_at IS NULL AND a.hidden = 0 AND a.live_of = '' GROUP BY k ORDER BY k DESC`, args...)
}

// narrowScope is the viewer's own photos from a place — a country, a state in it, or a city in that —
// of a person, or in an album.
func narrowScope(viewerID string, n Narrow) (string, []any) {
	where, args := `a.owner_id = ?`, []any{viewerID}
	if n.Place != "" {
		for i, part := range strings.SplitN(n.Place, "|", 3) {
			where += ` AND ` + []string{"e.country", "e.state", "e.city"}[i] + ` = ?`
			args = append(args, part)
		}
	}
	if n.Person != "" {
		where += ` AND a.id IN (SELECT asset_id FROM faces WHERE person_id = ? AND owner_id = ?)`
		args = append(args, n.Person, viewerID)
	}
	if n.Album != "" {
		where += ` AND a.id IN (SELECT asset_id FROM album_assets WHERE album_id = ?)`
		args = append(args, n.Album)
	}
	return where, args
}

// LocationsOf counts one person's photos by the storage location they lie in. Key is the location's id.
func (s *Store) LocationsOf(ctx context.Context, ownerID string) ([]Count, error) {
	return s.counts(ctx, `SELECT a.location, count(*), '' FROM assets a WHERE a.owner_id = ? AND a.deleted_at IS NULL AND a.hidden = 0 AND a.live_of = ''
		GROUP BY a.location ORDER BY count(*) DESC, a.location`, ownerID)
}

// Sharers lists the people who share photos, most photos first. Key is the person's id, Cover their name.
func (s *Store) Sharers(ctx context.Context) ([]Count, error) {
	return s.counts(ctx, `SELECT u.id, count(*), CASE WHEN u.name != '' THEN u.name ELSE u.email END FROM assets a JOIN users u ON u.id = a.owner_id
		WHERE a.shared = 1 AND a.deleted_at IS NULL AND a.hidden = 0 AND a.live_of = '' GROUP BY u.id ORDER BY count(*) DESC, u.id`)
}

// SetShared shows the owner's photos to everyone on this server, or stops showing them. It reports how
// many changed. Photos of other people are left alone.
func (s *Store) SetShared(ctx context.Context, ownerID string, ids []string, shared bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE assets SET shared = ? WHERE owner_id = ? AND (id = ? OR live_of = ?) AND shared != ?`, shared, ownerID, id, id, shared) // a live photo's moving half goes with it
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		changed += int(n)
	}
	return changed, tx.Commit()
}

// Places lists the towns and districts the owner has photos from, most photos first. Key is "country|state|city".
func (s *Store) Places(ctx context.Context, ownerID string) ([]Count, error) {
	return s.counts(ctx, `SELECT e.country || '|' || e.state || '|' || e.city AS k, count(*), max(a.id) FROM assets a JOIN asset_exif e ON e.asset_id = a.id
		WHERE a.owner_id = ? AND a.deleted_at IS NULL AND a.hidden = 0 AND a.live_of = '' AND e.city IS NOT NULL GROUP BY k ORDER BY count(*) DESC, k`, ownerID)
}

// People lists the owner's people — those in view, or those put away — with how many photos each is in.
// Key is the person's id; Cover carries the person's name.
func (s *Store) People(ctx context.Context, ownerID string, hidden bool) ([]Count, error) {
	return s.counts(ctx, `SELECT p.id, count(DISTINCT f.asset_id), p.name FROM persons p JOIN faces f ON f.person_id = p.id
		JOIN assets a ON a.id = f.asset_id AND a.deleted_at IS NULL AND a.hidden = 0
		WHERE p.owner_id = ? AND p.hidden = ? GROUP BY p.id ORDER BY count(DISTINCT f.asset_id) DESC, p.id`, ownerID, hidden)
}

func (s *Store) counts(ctx context.Context, query string, args ...any) ([]Count, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Key, &c.Count, &c.Cover); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AssetsWhere lists the photos of one month ("2026-09"), newest first: of the kind show, by and at say,
// or, with n set, the owner's own of a place, a person or an album.
func (s *Store) AssetsWhere(ctx context.Context, ownerID, month string, n Narrow, show, by, at string, limit int) ([]Asset, error) {
	where, args := scope(ownerID, show, by, at)
	if !n.none() {
		where, args = narrowScope(ownerID, n)
	}
	query := assetSelect + ` WHERE ` + where + ` AND a.deleted_at IS NULL AND a.hidden = 0 AND a.live_of = '' AND substr(` + when + `, 1, 7) = ?`
	args = append(args, month)
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY `+when+` DESC, a.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type JobReport struct {
	Kind, State string
	Count       int
}

type Failure struct{ Name, Kind, Error string }

// JobReports counts jobs by kind and state; Failures names the files whose jobs gave up, and why.
func (s *Store) JobReports(ctx context.Context) ([]JobReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, state, count(*) FROM jobs GROUP BY kind, state ORDER BY kind, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobReport
	for rows.Next() {
		var r JobReport
		if err := rows.Scan(&r.Kind, &r.State, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Failures(ctx context.Context, ownerID string, limit int) ([]Failure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.original_name, j.kind, j.error FROM jobs j JOIN assets a ON a.id = j.asset_id
		WHERE j.state = 'failed' AND a.owner_id = ? ORDER BY j.id DESC LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var f Failure
		if err := rows.Scan(&f.Name, &f.Kind, &f.Error); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Totals is how much one person keeps here: number of files and their bytes.
// PeopleIn lists the people found in one of the owner's photos, leaving out those put away.
func (s *Store) PeopleIn(ctx context.Context, ownerID, assetID string) ([]Person, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT p.id, p.name FROM faces f JOIN persons p ON p.id = f.person_id
		WHERE f.asset_id = ? AND f.owner_id = ? AND p.hidden = 0 ORDER BY p.name = '', p.name, p.id`, assetID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AlbumsWith lists the owner's albums that hold the photo.
func (s *Store) AlbumsWith(ctx context.Context, ownerID, assetID string) ([]Album, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT b.id, b.name FROM albums b JOIN album_assets x ON x.album_id = b.id
		WHERE b.owner_id = ? AND x.asset_id = ? ORDER BY b.name, b.id`, ownerID, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Album
	for rows.Next() {
		var b Album
		if err := rows.Scan(&b.ID, &b.Name); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) Totals(ctx context.Context, ownerID string) (files int, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size), 0) FROM assets WHERE owner_id = ?`, ownerID).Scan(&files, &bytes)
	return
}

// ── accounts kept by this server itself ─────────────────────────────────────────────────────────────────────

// UserByEmail finds a person by the address they log in with, and the hash of their password.
func (s *Store) UserByEmail(ctx context.Context, email string) (u User, hash string, err error) {
	u, err = scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE lower(email) = lower(?)`, email))
	if err == nil {
		err = s.db.QueryRowContext(ctx, `SELECT password FROM users WHERE id = ?`, u.ID).Scan(&hash)
	}
	return
}

// SetPassword stores the hash of a person's password and ends the sessions they have open elsewhere.
func (s *Store) SetPassword(ctx context.Context, userID, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password = ? WHERE id = ?`, hash, userID)
	return err
}

// Setting reads a value kept in the settings; "" if none is.
func (s *Store) Setting(ctx context.Context, name string) string {
	var value string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, name).Scan(&value)
	return value
}

// SetSetting keeps a value in the settings; "" removes it.
func (s *Store) SetSetting(ctx context.Context, name, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE name = ?`, name)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, value)
	return err
}

// CountUsers says how many people have an account here.
func (s *Store) CountUsers(ctx context.Context) (n int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return
}

// ── live photos ─────────────────────────────────────────────────────────────────────────────────────────────

// A live photo is two files, a photo and a few seconds of video taken with it. The video is recorded as
// belonging to the photo and is from then on shown only through it.

// SetLive records that a video is the moving half of a photo. Both count as changed.
func (s *Store) SetLive(ctx context.Context, ownerID, videoID, photoID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, change := range []struct{ query, id string }{
		{`UPDATE assets SET live_of = ?, shared = (SELECT shared FROM assets WHERE id = ?), seq = ? WHERE owner_id = ? AND id = ? AND type = 'VIDEO'`, videoID},
		{`UPDATE assets SET seq = ? WHERE owner_id = ? AND id = ?`, photoID},
	} {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		args := []any{seq, ownerID, change.id}
		if change.id == videoID {
			args = append([]any{photoID, photoID}, args...)
		}
		if _, err := tx.ExecContext(ctx, change.query, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LiveParts lists the moving halves of the photos named.
func (s *Store) LiveParts(ctx context.Context, ownerID string, photoIDs []string) ([]string, error) {
	var out []string
	for _, id := range photoIDs {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM assets WHERE owner_id = ? AND live_of = ?`, ownerID, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var part string
			if err := rows.Scan(&part); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, part)
		}
		rows.Close()
	}
	return out, nil
}

// Sibling is a file lying beside another under the same name but for what follows the last dot.
type Sibling struct {
	ID, Type, Path, LiveOf string
	DurationMs             int64
	Paired                 bool // a photo that already has a moving half
}

// Siblings lists the owner's files in one location whose path is stem plus a dot and an ending.
func (s *Store) Siblings(ctx context.Context, ownerID, location, stem string) ([]Sibling, error) {
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(stem) + `.%`
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.type, a.path, a.live_of, a.duration_ms, EXISTS (SELECT 1 FROM assets v WHERE v.live_of = a.id AND v.live_of != '')
		FROM assets a WHERE a.owner_id = ? AND a.location = ? AND a.path LIKE ? ESCAPE '\'`, ownerID, location, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sibling
	for rows.Next() {
		var b Sibling
		if err := rows.Scan(&b.ID, &b.Type, &b.Path, &b.LiveOf, &b.DurationMs, &b.Paired); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ShortVideos lists the videos of a few seconds that belong to no photo yet — the ones that may be the
// moving half of a live photo.
func (s *Store) ShortVideos(ctx context.Context, maxMs int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM assets WHERE type = 'VIDEO' AND live_of = '' AND duration_ms > 0 AND duration_ms <= ?`, maxMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ── storage locations (#14) ─────────────────────────────────────────────────────────────────────────────────

// Location is a folder that holds photos. The data folder itself is the location with the empty id and
// is not in the table.
type Location struct {
	ID, Path string
	Kind     string // KindStore or KindFolder
	OwnerID  string // for a folder: whose library its photos join
	Paused   bool   // for a folder: switched off for now
	Photos   int    // files recorded there
	Bytes    int64  // and their size
}

const (
	KindStore  = "store"  // a place this program puts photos
	KindFolder = "folder" // a folder someone already had; read where it lies, never written to (#15)
)

// Locations lists the folders added beside the data folder, oldest first, and counts what every location
// holds — the data folder's count comes first, under the empty id.
func (s *Store) Locations(ctx context.Context) ([]Location, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.path, l.kind, l.owner_id, l.paused, count(a.id), coalesce(sum(a.size), 0) FROM
		(SELECT '' AS id, '' AS path, 'store' AS kind, '' AS owner_id, 0 AS paused, '' AS created_at UNION ALL SELECT id, path, kind, owner_id, paused, created_at FROM locations) l
		LEFT JOIN assets a ON a.location = l.id GROUP BY l.id ORDER BY l.created_at, l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		var l Location
		if err := rows.Scan(&l.ID, &l.Path, &l.Kind, &l.OwnerID, &l.Paused, &l.Photos, &l.Bytes); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AddLocation records a folder: a place for photos (KindStore), or a folder to read where it lies
// (KindFolder) for ownerID's library. A folder already recorded keeps its id and its kind; ErrDuplicate
// if that kind is the other one.
func (s *Store) AddLocation(ctx context.Context, path, kind, ownerID string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO locations (id, path, kind, owner_id, created_at) VALUES (?,?,?,?,?)`, NewID(), path, kind, ownerID, Timestamp(time.Now())); err != nil {
		return "", err
	}
	var id, was string
	if err := s.db.QueryRowContext(ctx, `SELECT id, kind FROM locations WHERE path = ?`, path).Scan(&id, &was); err != nil {
		return "", err
	}
	if was != kind {
		return "", ErrDuplicate
	}
	return id, nil
}

// Location finds one added folder.
func (s *Store) Location(ctx context.Context, id string) (l Location, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, path, kind, owner_id, paused FROM locations WHERE id = ?`, id).Scan(&l.ID, &l.Path, &l.Kind, &l.OwnerID, &l.Paused)
	return
}

// PauseLocation switches a read-only folder off, or on again. Off, its photos are shown nowhere and the
// owner's devices are told they are gone; the records and the small pictures stay, so that switching it
// on brings everything back at once.
func (s *Store) PauseLocation(ctx context.Context, id string, paused bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE locations SET paused = ? WHERE id = ? AND kind = 'folder'`, paused, id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, owner_id FROM assets WHERE location = ? AND hidden != ?`, id, paused)
	if err != nil {
		return err
	}
	var ids, owners []string
	for rows.Next() {
		var assetID, owner string
		if err := rows.Scan(&assetID, &owner); err != nil {
			rows.Close()
			return err
		}
		ids, owners = append(ids, assetID), append(owners, owner)
	}
	rows.Close()
	for i, assetID := range ids {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		// a new seq either way: switched on, the photo is sent to the devices again
		if _, err := tx.ExecContext(ctx, `UPDATE assets SET hidden = ?, seq = ? WHERE id = ?`, paused, seq, assetID); err != nil {
			return err
		}
		if paused {
			if _, err := tx.ExecContext(ctx, `INSERT INTO deletions (seq, owner_id, kind, a, b) VALUES (?,?,'asset',?,'')`, seq, owners[i], assetID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// FileAt is what the library records of one file in a location.
type FileAt struct {
	AssetID, OwnerID, Modified string
	Size                       int64
}

// FilesAt lists what is recorded in a location, by path.
func (s *Store) FilesAt(ctx context.Context, location string) (map[string]FileAt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, id, owner_id, file_modified_at, size FROM assets WHERE location = ?`, location)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileAt{}
	for rows.Next() {
		var path string
		var f FileAt
		if err := rows.Scan(&path, &f.AssetID, &f.OwnerID, &f.Modified, &f.Size); err != nil {
			return nil, err
		}
		out[path] = f
	}
	return out, rows.Err()
}

// MoveFile records that a file in a read-only folder now lies under another path or name. The photo
// stays the same photo: its small pictures, albums and marks stay with it.
func (s *Store) MoveFile(ctx context.Context, assetID, path, name, modified string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET path = ?, original_name = ?, file_modified_at = ?, seq = ? WHERE id = ?`, path, name, modified, seq, assetID); err != nil {
		return err
	}
	return tx.Commit()
}

// Skip notes that a file in a read-only folder is not to be read in again.
func (s *Store) Skip(ctx context.Context, location, path string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skipped (location, path) VALUES (?,?)`, location, path)
	return err
}

// Skipped lists the files of a folder that are not to be read in.
func (s *Store) Skipped(ctx context.Context, location string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM skipped WHERE location = ?`, location)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = true
	}
	return out, rows.Err()
}

// RemoveLocation forgets a folder that holds no photos; one that holds any stays.
func (s *Store) RemoveLocation(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM locations WHERE id = ? AND id NOT IN (SELECT location FROM assets)`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = s.db.ExecContext(ctx, `DELETE FROM skipped WHERE location = ?`, id)
	}
	return err
}

// DefaultLocation is where new photos go when their owner has no place of their own.
func (s *Store) DefaultLocation(ctx context.Context) (id string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = 'location' AND (value = '' OR value IN (SELECT id FROM locations WHERE kind = 'store'))`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return
}

func (s *Store) SetDefaultLocation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) SELECT 'location', ? WHERE ? = '' OR ? IN (SELECT id FROM locations WHERE kind = 'store')
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, id, id, id)
	return err
}

// FacesOn says whether faces are looked for and grouped into people. On at first (operator, 2026-10-15).
func (s *Store) FacesOn(ctx context.Context) bool {
	var on string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = 'faces'`).Scan(&on)
	return on != "off"
}

func (s *Store) SetFacesOn(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES ('faces', ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, value)
	return err
}

// ForgetJobs removes the jobs of one kind in the given states, so that they can be queued afresh.
func (s *Store) ForgetJobs(ctx context.Context, kind string, states ...string) error {
	for _, state := range states {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE kind = ? AND state = ?`, kind, state); err != nil {
			return err
		}
	}
	return nil
}

// CouponsOn says whether coupons are looked for and gathered into an album of their own. Off at first.
func (s *Store) CouponsOn(ctx context.Context) bool {
	var on string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = 'coupons'`).Scan(&on)
	return on == "on"
}

func (s *Store) SetCouponsOn(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES ('coupons', ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, value)
	return err
}

// CouponCandidates lists the pictures the coupon job has never looked at that could be coupons: pictures
// that say nothing of a camera — screenshots, and images saved from a message.
func (s *Store) CouponCandidates(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id FROM assets a JOIN asset_exif e ON e.asset_id = a.id
		WHERE a.type = 'IMAGE' AND a.live_of = '' AND a.deleted_at IS NULL AND coalesce(e.make, '') = ''
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.asset_id = a.id AND j.kind = 'coupon')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AddCoupon records that a picture is a coupon. It reports false if that was known already.
func (s *Store) AddCoupon(ctx context.Context, assetID, code, expires string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO coupons (asset_id, code, expires) VALUES (?,?,?) ON CONFLICT DO NOTHING`, assetID, code, expires)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AlbumCoupons lists the photos in one of the owner's albums with the last day each coupon can be used,
// "" for a photo not known as a coupon.
func (s *Store) AlbumCoupons(ctx context.Context, ownerID, albumID string) ([]Asset, map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, assetSelect+` WHERE a.owner_id = ? AND a.deleted_at IS NULL AND a.hidden = 0
		AND a.id IN (SELECT asset_id FROM album_assets WHERE album_id = ?)`, ownerID, albumID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var list []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, nil, err
		}
		list = append(list, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	until := map[string]string{}
	found, err := s.db.QueryContext(ctx, `SELECT c.asset_id, c.expires FROM coupons c JOIN album_assets x ON x.asset_id = c.asset_id WHERE x.album_id = ?`, albumID)
	if err != nil {
		return nil, nil, err
	}
	defer found.Close()
	for found.Next() {
		var id, day string
		if err := found.Scan(&id, &day); err != nil {
			return nil, nil, err
		}
		until[id] = day
	}
	return list, until, found.Err()
}

type Memory struct {
	ID, OwnerID, Day, CreatedAt string
	Year                        int
	Seq                         int64
}

// shown is a photo that is in the library: in view, not in the trash, not the moving half of a live photo.
const shown = `a.deleted_at IS NULL AND a.hidden = 0 AND a.live_of = ''`

// MonthYears lists the years before the given one in which the owner has photos from this month ("10").
func (s *Store) MonthYears(ctx context.Context, ownerID, month string, before int) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT CAST(substr(`+when+`, 1, 4) AS INTEGER) AS y FROM assets a LEFT JOIN asset_exif e ON e.asset_id = a.id
		WHERE a.owner_id = ? AND `+shown+` AND substr(`+when+`, 6, 2) = ? AND y < ? ORDER BY y`, ownerID, month, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var years []int
	for rows.Next() {
		var y int
		if err := rows.Scan(&y); err != nil {
			return nil, err
		}
		years = append(years, y)
	}
	return years, rows.Err()
}

// MonthPhotos lists the owner's photos of one month ("2023-10"), oldest first.
func (s *Store) MonthPhotos(ctx context.Context, ownerID, month string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id FROM assets a LEFT JOIN asset_exif e ON e.asset_id = a.id
		WHERE a.owner_id = ? AND `+shown+` AND substr(`+when+`, 1, 7) = ? ORDER BY `+when+`, a.id`, ownerID, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// HasMemory says whether the owner has the memory of that day already.
func (s *Store) HasMemory(ctx context.Context, ownerID, day string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE owner_id = ? AND day = ?`, ownerID, day).Scan(&n)
	return n > 0, err
}

// AddMemory records the memory of a day. A day that has one keeps it.
func (s *Store) AddMemory(ctx context.Context, ownerID, day string, year int, assetIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	id := NewID()
	res, err := tx.ExecContext(ctx, `INSERT INTO memories (id, owner_id, day, year, created_at, seq) VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		id, ownerID, day, year, Timestamp(time.Now()), seq)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	for _, asset := range assetIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_assets (memory_id, asset_id, seq) VALUES (?,?,?)`, id, asset, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RetireMemories takes the memories of every day but the given one off the devices: the app lists every
// memory it has, whatever day it is for — past ones, and ones an earlier version made days ahead. They
// stay here, marked gone, for the years they showed. It returns whose memories went.
func (s *Store) RetireMemories(ctx context.Context, today string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, owner_id FROM memories WHERE gone = 0 AND day <> ?`, today)
	if err != nil {
		return nil, err
	}
	type memory struct{ id, owner string }
	var past []memory
	for rows.Next() {
		var m memory
		if err := rows.Scan(&m.id, &m.owner); err != nil {
			rows.Close()
			return nil, err
		}
		past = append(past, m)
	}
	rows.Close()
	owners := map[string]bool{}
	for _, m := range past {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET gone = 1 WHERE id = ?`, m.id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deletions (seq, owner_id, kind, a, b) VALUES (?,?,'memory',?,'')`, seq, m.owner, m.id); err != nil {
			return nil, err
		}
		owners[m.owner] = true
	}
	var out []string
	for o := range owners {
		out = append(out, o)
	}
	return out, tx.Commit()
}

// MemoryYears lists the years the owner's memories showed on the days from one day up to another
// (inclusive) — gone ones too.
func (s *Store) MemoryYears(ctx context.Context, ownerID, from, to string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT year FROM memories WHERE owner_id = ? AND day >= ? AND day <= ?`, ownerID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var years []int
	for rows.Next() {
		var y int
		if err := rows.Scan(&y); err != nil {
			return nil, err
		}
		years = append(years, y)
	}
	return years, rows.Err()
}

// MemoriesSince walks the owner's memories made after seq, but those whose day is past.
func (s *Store) MemoriesSince(ctx context.Context, ownerID string, seq int64, each func(Memory) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner_id, day, created_at, year, seq FROM memories WHERE owner_id = ? AND seq > ? AND gone = 0 ORDER BY seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.OwnerID, &m.Day, &m.CreatedAt, &m.Year, &m.Seq); err != nil {
			return err
		}
		if err := each(m); err != nil {
			return err
		}
	}
	return rows.Err()
}

// MemoryAssetsSince walks the photos put in the owner's memories after seq.
func (s *Store) MemoryAssetsSince(ctx context.Context, ownerID string, seq int64, each func(memoryID, assetID string, seq int64) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT x.memory_id, x.asset_id, x.seq FROM memory_assets x JOIN memories m ON m.id = x.memory_id
		WHERE m.owner_id = ? AND x.seq > ? AND m.gone = 0 ORDER BY x.seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var memoryID, assetID string
		var at int64
		if err := rows.Scan(&memoryID, &assetID, &at); err != nil {
			return err
		}
		if err := each(memoryID, assetID, at); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AutoAlbum is the owner's album the server fills by itself with one kind of picture ("coupons"),
// made now with the given name if there is none — or if the owner deleted it.
func (s *Store) AutoAlbum(ctx context.Context, ownerID, kind, name string) (Album, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM albums WHERE owner_id = ? AND auto = ? ORDER BY created_at LIMIT 1`, ownerID, kind).Scan(&id)
	if err == nil {
		return s.Album(ctx, ownerID, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Album{}, err
	}
	b, err := s.CreateAlbum(ctx, ownerID, name, "")
	if err != nil {
		return Album{}, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE albums SET auto = ? WHERE id = ?`, kind, b.ID)
	return b, err
}

// HomeLocation, as a person's choice, means the data folder; the empty choice means "wherever the default is".
const HomeLocation = "home"

// SetUserLocation says where one person's new photos go: a location's id, HomeLocation, or "" to follow
// the default.
func (s *Store) SetUserLocation(ctx context.Context, userID, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET location = ? WHERE id = ? AND (? IN ('', 'home') OR ? IN (SELECT id FROM locations WHERE kind = 'store'))`, id, userID, id, id)
	return err
}

// LocationFor is where a person's next photo goes.
func (s *Store) LocationFor(ctx context.Context, userID string) (string, error) {
	var own string
	err := s.db.QueryRowContext(ctx, `SELECT location FROM users WHERE id = ? AND (location = 'home' OR location IN (SELECT id FROM locations WHERE kind = 'store'))`, userID).Scan(&own)
	if err == nil {
		if own == HomeLocation {
			own = ""
		}
		return own, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return s.DefaultLocation(ctx)
}

// Users lists everyone who has come in, in the order they came.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ── albums ──────────────────────────────────────────────────────────────────────────────────────────────────

type Album struct {
	ID, OwnerID, Name, Description, CoverID string
	Auto                                    string // what the server fills it with by itself: "coupons"; "" for one a person made
	CreatedAt, UpdatedAt                    string
	Count                                   int
	Seq                                     int64
}

type Deletion struct {
	Seq  int64
	A, B string
}

const albumSelect = `SELECT b.id, b.owner_id, b.name, b.description,
	coalesce(b.cover_id, (SELECT asset_id FROM album_assets x WHERE x.album_id = b.id ORDER BY x.seq DESC LIMIT 1), ''),
	b.auto, b.created_at, b.updated_at, (SELECT count(*) FROM album_assets x WHERE x.album_id = b.id), b.seq FROM albums b`

func scanAlbum(row scanner) (b Album, err error) {
	err = row.Scan(&b.ID, &b.OwnerID, &b.Name, &b.Description, &b.CoverID, &b.Auto, &b.CreatedAt, &b.UpdatedAt, &b.Count, &b.Seq)
	return
}

// Album returns one of the owner's albums; sql.ErrNoRows for anyone else's.
func (s *Store) Album(ctx context.Context, ownerID, id string) (Album, error) {
	return scanAlbum(s.db.QueryRowContext(ctx, albumSelect+` WHERE b.owner_id = ? AND b.id = ?`, ownerID, id))
}

// AlbumsSince walks the owner's albums changed after seq; with seq 0, all of them.
func (s *Store) AlbumsSince(ctx context.Context, ownerID string, seq int64, each func(Album) error) error {
	rows, err := s.db.QueryContext(ctx, albumSelect+` WHERE b.owner_id = ? AND b.seq > ? ORDER BY b.seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanAlbum(rows)
		if err != nil {
			return err
		}
		if err := each(b); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) CreateAlbum(ctx context.Context, ownerID, name, description string) (Album, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Album{}, err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return Album{}, err
	}
	id, now := NewID(), Timestamp(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO albums (id, owner_id, name, description, created_at, updated_at, seq) VALUES (?,?,?,?,?,?,?)`,
		id, ownerID, name, description, now, now, seq); err != nil {
		return Album{}, err
	}
	if err := tx.Commit(); err != nil {
		return Album{}, err
	}
	return s.Album(ctx, ownerID, id)
}

// UpdateAlbum changes what is not nil. The cover must be a photo in the album.
func (s *Store) UpdateAlbum(ctx context.Context, ownerID, id string, name, description, coverID *string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE albums SET name = coalesce(?, name), description = coalesce(?, description),
		cover_id = CASE WHEN ? IS NOT NULL AND EXISTS (SELECT 1 FROM album_assets WHERE album_id = albums.id AND asset_id = ?) THEN ? ELSE cover_id END,
		updated_at = ?, seq = ? WHERE owner_id = ? AND id = ?`, name, description, coverID, coverID, coverID, Timestamp(time.Now()), seq, ownerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// DeleteAlbum removes an album and leaves its photos alone.
func (s *Store) DeleteAlbum(ctx context.Context, ownerID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM shared_link_assets WHERE link_id IN (SELECT id FROM shared_links WHERE album_id IN (SELECT id FROM albums WHERE owner_id = ? AND id = ?))`,
		`DELETE FROM shared_links WHERE album_id IN (SELECT id FROM albums WHERE owner_id = ? AND id = ?)`, // its links go with it
		`DELETE FROM album_assets WHERE album_id IN (SELECT id FROM albums WHERE owner_id = ? AND id = ?)`} {
		if _, err := tx.ExecContext(ctx, q, ownerID, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM albums WHERE owner_id = ? AND id = ?`, ownerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deletions (seq, owner_id, kind, a) VALUES (?,?,'album',?)`, seq, ownerID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ChangeAlbumAssets puts photos into an album (add) or takes them out. The answer has one entry per id:
// "" for done, or why not — "not_found" for a photo that is not the owner's, "duplicate" for one already
// in (or, when taking out, one that was not in).
func (s *Store) ChangeAlbumAssets(ctx context.Context, ownerID, albumID string, assetIDs []string, add bool) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM albums WHERE owner_id = ? AND id = ?`, ownerID, albumID).Scan(&found); err != nil {
		return nil, err
	}
	if found == 0 {
		return nil, sql.ErrNoRows
	}
	results := make([]string, len(assetIDs))
	changed := false
	for i, assetID := range assetIDs {
		var mine int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM assets WHERE owner_id = ? AND id = ?`, ownerID, assetID).Scan(&mine); err != nil {
			return nil, err
		}
		if mine == 0 {
			results[i] = "not_found"
			continue
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return nil, err
		}
		var res sql.Result
		if add {
			res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO album_assets (album_id, asset_id, seq) VALUES (?,?,?)`, albumID, assetID, seq)
		} else {
			res, err = tx.ExecContext(ctx, `DELETE FROM album_assets WHERE album_id = ? AND asset_id = ?`, albumID, assetID)
		}
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			results[i] = "duplicate"
			continue
		}
		changed = true
		if !add {
			if _, err := tx.ExecContext(ctx, `INSERT INTO deletions (seq, owner_id, kind, a, b) VALUES (?,?,'album_asset',?,?)`, seq, ownerID, albumID, assetID); err != nil {
				return nil, err
			}
		}
	}
	if changed { // the album itself counts as changed: its size and perhaps its cover did
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET updated_at = ?, seq = ?,
			cover_id = CASE WHEN cover_id IN (SELECT asset_id FROM album_assets WHERE album_id = albums.id) THEN cover_id END WHERE id = ?`,
			Timestamp(time.Now()), seq, albumID); err != nil {
			return nil, err
		}
	}
	return results, tx.Commit()
}

// AlbumAssetsSince walks the (album, photo) pairs of the owner's albums added after seq.
func (s *Store) AlbumAssetsSince(ctx context.Context, ownerID string, seq int64, each func(albumID, assetID string, seq int64) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT x.album_id, x.asset_id, x.seq FROM album_assets x JOIN albums b ON b.id = x.album_id
		WHERE b.owner_id = ? AND x.seq > ? ORDER BY x.seq`, ownerID, seq)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var albumID, assetID string
		var at int64
		if err := rows.Scan(&albumID, &assetID, &at); err != nil {
			return err
		}
		if err := each(albumID, assetID, at); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DeletionsSince lists what of one kind has gone since seq.
func (s *Store) DeletionsSince(ctx context.Context, ownerID, kind string, seq int64) ([]Deletion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, a, b FROM deletions WHERE owner_id = ? AND kind = ? AND seq > ? ORDER BY seq`, ownerID, kind, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deletion
	for rows.Next() {
		var d Deletion
		if err := rows.Scan(&d.Seq, &d.A, &d.B); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetFavorite marks the owner's photos as favourites, or unmarks them, and reports how many changed.
func (s *Store) SetFavorite(ctx context.Context, ownerID string, ids []string, favorite bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	for _, id := range ids {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE assets SET favorite = ?, seq = ? WHERE owner_id = ? AND id = ? AND favorite != ?`, favorite, seq, ownerID, id, favorite)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		changed += int(n)
	}
	return changed, tx.Commit()
}

// ── the trash ───────────────────────────────────────────────────────────────────────────────────────────────

// SetTrashed moves the owner's photos into the trash or back out; with no ids and trashed false, it
// brings back everything. It reports how many changed. Nothing on disk is touched.
func (s *Store) SetTrashed(ctx context.Context, ownerID string, ids []string, trashed bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if ids == nil && !trashed {
		if ids, err = trashedIDs(ctx, tx, `owner_id = ?`, ownerID); err != nil {
			return 0, err
		}
	}
	var at any
	if trashed {
		at = Timestamp(time.Now())
	}
	changed := 0
	for _, id := range ids {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE assets SET deleted_at = ?, seq = ? WHERE owner_id = ? AND id = ? AND (deleted_at IS NULL) = ?`, at, seq, ownerID, id, trashed)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		changed += int(n)
	}
	return changed, tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func trashedIDs(ctx context.Context, q queryer, where string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM assets WHERE deleted_at IS NOT NULL AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Trashed lists the ids of the owner's photos in the trash.
func (s *Store) Trashed(ctx context.Context, ownerID string) ([]string, error) {
	return trashedIDs(ctx, s.db, `owner_id = ?`, ownerID)
}

// TrashedBefore lists, for every owner, the photos that went into the trash before the given moment.
func (s *Store) TrashedBefore(ctx context.Context, before time.Time) (owners, ids []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id, id FROM assets WHERE deleted_at IS NOT NULL AND deleted_at < ?`, Timestamp(before))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, id string
		if err := rows.Scan(&owner, &id); err != nil {
			return nil, nil, err
		}
		owners, ids = append(owners, owner), append(ids, id)
	}
	return owners, ids, rows.Err()
}

// TrashedAssets lists the owner's photos in the trash, most recently trashed first.
func (s *Store) TrashedAssets(ctx context.Context, ownerID string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, assetSelect+` WHERE a.owner_id = ? AND a.deleted_at IS NOT NULL AND a.hidden = 0 AND a.live_of = '' ORDER BY a.deleted_at DESC LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── deleting ────────────────────────────────────────────────────────────────────────────────────────────────

// Removed is what is left to clean up on disk after a photo's record is gone.
type Removed struct {
	AssetID, Location, Path string
	// People whose shown face went with the photo: person id -> the face to show instead, or "" if the
	// person had no other face and is gone too.
	People map[string]string
}

// DeleteAsset removes one of the owner's photos from the record: the photo, what was read from it, its
// place in albums, its faces, its jobs. A person keeps existing as long as one face is left.
// sql.ErrNoRows if the photo is not the owner's. The files are the caller's to remove.
func (s *Store) DeleteAsset(ctx context.Context, ownerID, assetID string) (Removed, error) {
	out := Removed{AssetID: assetID, People: map[string]string{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT location, path FROM assets WHERE owner_id = ? AND id = ?`, ownerID, assetID).Scan(&out.Location, &out.Path); err != nil {
		return out, err
	}
	gone := func(kind, a, b string) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO deletions (seq, owner_id, kind, a, b) VALUES (?,?,?,?,?)`, seq, ownerID, kind, a, b)
		return err
	}
	// faces, and the people they belonged to
	rows, err := tx.QueryContext(ctx, `SELECT id, coalesce(person_id, '') FROM faces WHERE asset_id = ?`, assetID)
	if err != nil {
		return out, err
	}
	var faceIDs []string
	people := map[string]bool{}
	for rows.Next() {
		var faceID, personID string
		if err := rows.Scan(&faceID, &personID); err != nil {
			rows.Close()
			return out, err
		}
		faceIDs = append(faceIDs, faceID)
		if personID != "" {
			people[personID] = true
		}
	}
	rows.Close()
	for _, faceID := range faceIDs {
		if err := gone("face", faceID, ""); err != nil {
			return out, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM faces WHERE asset_id = ?`, assetID); err != nil {
		return out, err
	}
	for personID := range people {
		var shown string
		var next sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT p.face_id, (SELECT id FROM faces WHERE person_id = p.id ORDER BY (x2 - x1) DESC LIMIT 1) FROM persons p WHERE p.id = ?`, personID).Scan(&shown, &next); err != nil {
			return out, err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return out, err
		}
		switch {
		case !next.Valid: // nobody left to show
			if _, err := tx.ExecContext(ctx, `DELETE FROM persons WHERE id = ?`, personID); err != nil {
				return out, err
			}
			if err := gone("person", personID, ""); err != nil {
				return out, err
			}
			out.People[personID] = ""
		default:
			stillThere := 0
			tx.QueryRowContext(ctx, `SELECT count(*) FROM faces WHERE id = ?`, shown).Scan(&stillThere)
			if stillThere == 0 {
				shown = next.String
				out.People[personID] = shown
			}
			if _, err := tx.ExecContext(ctx, `UPDATE persons SET face_id = ?, updated_at = ?, seq = ? WHERE id = ?`, shown, Timestamp(time.Now()), seq, personID); err != nil {
				return out, err
			}
		}
	}
	// albums it was in count as changed
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET seq = ?, updated_at = ?, cover_id = CASE WHEN cover_id = ? THEN NULL ELSE cover_id END
		WHERE id IN (SELECT album_id FROM album_assets WHERE asset_id = ?)`, seq, Timestamp(time.Now()), assetID, assetID); err != nil {
		return out, err
	}
	for _, q := range []string{`DELETE FROM album_assets WHERE asset_id = ?`, `DELETE FROM asset_exif WHERE asset_id = ?`, `DELETE FROM coupons WHERE asset_id = ?`, `DELETE FROM memory_assets WHERE asset_id = ?`,
		`DELETE FROM shared_link_assets WHERE asset_id = ?`, `DELETE FROM jobs WHERE asset_id = ?`, `DELETE FROM assets WHERE id = ?`} {
		if _, err := tx.ExecContext(ctx, q, assetID); err != nil {
			return out, err
		}
	}
	if err := gone("asset", assetID, ""); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
