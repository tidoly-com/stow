package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Listing photos looks up each one's moving half through the index of moving halves: a month of 11,000
// photos took 50 s when every lookup read every photo (2026-10-18).
func TestMovingHalvesAreFoundThroughTheirIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "stow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + assetSelect + ` WHERE a.owner_id = 'x' ORDER BY ` + when)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")
	if strings.Contains(joined, "SCAN v") || !strings.Contains(joined, "assets_live") {
		t.Errorf("plan: %s", joined)
	}
}
