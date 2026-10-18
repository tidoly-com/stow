package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// SharedLink is an address that shows photos to someone without an account on this server: one album as
// it is when opened, or photos chosen one by one. Only the owner's own photos are shown through it.
type SharedLink struct {
	ID, Key, OwnerID string
	AlbumID          string // "" for photos chosen one by one
	Description      string
	Password         string // a hash; "" for none
	ExpiresAt        string // "" for never
	AllowDownload    bool
	ShowMetadata     bool
	CreatedAt        string
}

// Expired says whether the link no longer opens.
func (l SharedLink) Expired(now time.Time) bool {
	if l.ExpiresAt == "" {
		return false
	}
	until, err := time.Parse(timeLayout, l.ExpiresAt)
	return err != nil || !now.Before(until)
}

const linkSelect = `SELECT id, key, owner_id, coalesce(album_id, ''), description, password, coalesce(expires_at, ''), allow_download, show_metadata, created_at FROM shared_links`

func scanLink(row scanner) (l SharedLink, err error) {
	err = row.Scan(&l.ID, &l.Key, &l.OwnerID, &l.AlbumID, &l.Description, &l.Password, &l.ExpiresAt, &l.AllowDownload, &l.ShowMetadata, &l.CreatedAt)
	return l, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AddSharedLink records a new link. For photos chosen one by one, only the owner's own that are not in the
// trash are taken; ErrNoRows when none is.
func (s *Store) AddSharedLink(ctx context.Context, l SharedLink, assetIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if l.AlbumID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM albums WHERE id = ? AND owner_id = ?`, l.AlbumID, l.OwnerID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return sql.ErrNoRows
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO shared_links (id, key, owner_id, album_id, description, password, expires_at, allow_download, show_metadata, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, l.ID, l.Key, l.OwnerID, nullIfEmpty(l.AlbumID), l.Description, l.Password, nullIfEmpty(l.ExpiresAt),
		l.AllowDownload, l.ShowMetadata, l.CreatedAt); err != nil {
		return err
	}
	if l.AlbumID == "" {
		added, err := addLinkAssets(ctx, tx, l, assetIDs)
		if err != nil {
			return err
		}
		if added == 0 {
			return sql.ErrNoRows
		}
	}
	return tx.Commit()
}

func addLinkAssets(ctx context.Context, tx *sql.Tx, l SharedLink, assetIDs []string) (int, error) {
	added := 0
	for _, id := range assetIDs {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO shared_link_assets (link_id, asset_id)
			SELECT ?, id FROM assets WHERE id = ? AND owner_id = ? AND deleted_at IS NULL AND hidden = 0`, l.ID, id, l.OwnerID)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

// SharedLinks lists a person's links, newest first; with albumID, only that album's.
func (s *Store) SharedLinks(ctx context.Context, ownerID, albumID string) ([]SharedLink, error) {
	query, args := linkSelect+` WHERE owner_id = ?`, []any{ownerID}
	if albumID != "" {
		query, args = query+` AND album_id = ?`, append(args, albumID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SharedLink is one of a person's links.
func (s *Store) SharedLink(ctx context.Context, ownerID, id string) (SharedLink, error) {
	return scanLink(s.db.QueryRowContext(ctx, linkSelect+` WHERE owner_id = ? AND id = ?`, ownerID, id))
}

// SharedLinkByKey is the link an address names, for whoever holds it.
func (s *Store) SharedLinkByKey(ctx context.Context, key string) (SharedLink, error) {
	if key == "" {
		return SharedLink{}, sql.ErrNoRows
	}
	return scanLink(s.db.QueryRowContext(ctx, linkSelect+` WHERE key = ?`, key))
}

// UpdateSharedLink keeps what can change of a link: its words, password, end and what it lets one do.
func (s *Store) UpdateSharedLink(ctx context.Context, l SharedLink) error {
	res, err := s.db.ExecContext(ctx, `UPDATE shared_links SET description = ?, password = ?, expires_at = ?, allow_download = ?, show_metadata = ?
		WHERE owner_id = ? AND id = ?`, l.Description, l.Password, nullIfEmpty(l.ExpiresAt), l.AllowDownload, l.ShowMetadata, l.OwnerID, l.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteSharedLink ends a link: its address no longer opens.
func (s *Store) DeleteSharedLink(ctx context.Context, ownerID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM shared_link_assets WHERE link_id IN (SELECT id FROM shared_links WHERE owner_id = ? AND id = ?)`, ownerID, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM shared_links WHERE owner_id = ? AND id = ?`, ownerID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// ChangeSharedLinkAssets puts photos into a link of photos chosen one by one, or takes them out. The answer
// has one entry per id: "" for done, or why not — "not_found", "duplicate".
func (s *Store) ChangeSharedLinkAssets(ctx context.Context, l SharedLink, assetIDs []string, add bool) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]string, len(assetIDs))
	for i, id := range assetIDs {
		var res sql.Result
		if add {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM shared_link_assets WHERE link_id = ? AND asset_id = ?`, l.ID, id).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				out[i] = "duplicate"
				continue
			}
			if added, err := addLinkAssets(ctx, tx, l, []string{id}); err != nil {
				return nil, err
			} else if added == 0 {
				out[i] = "not_found"
			}
			continue
		}
		if res, err = tx.ExecContext(ctx, `DELETE FROM shared_link_assets WHERE link_id = ? AND asset_id = ?`, l.ID, id); err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			out[i] = "not_found"
		}
	}
	return out, tx.Commit()
}

// linkScope is the photos a link shows: the album's, or those chosen — the owner's own, not in the trash.
// With halves, the moving half of a live photo counts too, so that it plays.
func linkScope(l SharedLink, halves bool) (string, []any) {
	in := `SELECT asset_id FROM shared_link_assets WHERE link_id = ?`
	arg := l.ID
	if l.AlbumID != "" {
		in, arg = `SELECT asset_id FROM album_assets WHERE album_id = ?`, l.AlbumID
	}
	where := `a.owner_id = ? AND a.deleted_at IS NULL AND a.hidden = 0 AND (a.id IN (` + in + `)`
	args := []any{l.OwnerID, arg}
	if halves {
		where += ` OR a.live_of IN (` + in + `)`
		args = append(args, arg)
	}
	return where + `)`, args
}

// SharedLinkAssets lists what a link shows, newest first, the moving halves of live photos left out.
func (s *Store) SharedLinkAssets(ctx context.Context, l SharedLink, limit int) ([]Asset, error) {
	where, args := linkScope(l, false)
	rows, err := s.db.QueryContext(ctx, assetSelect+` WHERE `+where+` AND a.live_of = '' ORDER BY `+when+` DESC, a.id LIMIT ?`, append(args, limit)...)
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

// SharedLinkAsset is one photo a link shows; ErrNoRows for any other.
func (s *Store) SharedLinkAsset(ctx context.Context, l SharedLink, id string) (Asset, error) {
	where, args := linkScope(l, true)
	return scanAsset(s.db.QueryRowContext(ctx, assetSelect+` WHERE a.id = ? AND `+where, append([]any{id}, args...)...))
}

// LinkTitle is what a link is called on its page: its words, else its album's name.
func (s *Store) LinkTitle(ctx context.Context, l SharedLink) string {
	if strings.TrimSpace(l.Description) != "" || l.AlbumID == "" {
		return l.Description
	}
	var name string
	s.db.QueryRowContext(ctx, `SELECT name FROM albums WHERE id = ?`, l.AlbumID).Scan(&name)
	return name
}
