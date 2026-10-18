package store

import "context"

// ScreenshotsOn says whether screenshots are gathered into an album of their own: on unless switched off.
func (s *Store) ScreenshotsOn(ctx context.Context) bool {
	var on string
	s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = 'screenshots'`).Scan(&on)
	return on != "off"
}

func (s *Store) SetScreenshotsOn(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES ('screenshots', ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, value)
	return err
}

// ScreenshotCandidates lists the pictures already read that could be screenshots and were never put in the
// screenshot album: those that say nothing of a camera.
func (s *Store) ScreenshotCandidates(ctx context.Context) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, assetSelect+` WHERE a.type = 'IMAGE' AND a.live_of = '' AND a.deleted_at IS NULL AND a.hidden = 0
		AND e.asset_id IS NOT NULL AND coalesce(e.make, '') = '' AND a.id NOT IN (SELECT asset_id FROM screenshots)`)
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

// AddScreenshot records a picture as put in the screenshot album; false when it was put there before — and
// so, if it is not there now, its owner took it out.
func (s *Store) AddScreenshot(ctx context.Context, assetID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO screenshots (asset_id) VALUES (?) ON CONFLICT DO NOTHING`, assetID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
