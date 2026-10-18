package store

import (
	"context"
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

// A job someone waits on with the page open is handed out before the rest, and found through its own
// index: behind a folder of 20,000 photos being read, it would otherwise come hours later.
func TestAskedJobsGoFirst(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "stow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	u, err := s.UpsertUser(ctx, "sub-owner", "owner@example.com", "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := s.AddAsset(ctx, &Asset{ID: id, OwnerID: u.ID, Checksum: []byte(id), OriginalName: id + ".heic", Path: id + ".heic", Type: "IMAGE"}, []string{"thumbnail"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if state, err := s.AskJob(ctx, "c", AskedKind); err != nil || state != "queued" {
			t.Fatalf("ask: %q %v", state, err)
		}
	}
	var got []string
	for {
		j, err := s.ClaimJob(ctx, []string{"thumbnail", AskedKind})
		if err != nil {
			break
		}
		got = append(got, j.AssetID+":"+j.Kind)
	}
	if strings.Join(got, " ") != "c:full a:thumbnail b:thumbnail c:thumbnail" {
		t.Errorf("handed out: %v — asked twice, it must be there once and first", got)
	}
	var detail string
	s.db.QueryRow(`EXPLAIN QUERY PLAN SELECT id FROM jobs WHERE state = 'queued' AND kind = '`+AskedKind+`' ORDER BY id LIMIT 1`).Scan(new(int), new(int), new(int), &detail)
	if !strings.Contains(detail, "jobs_kind") {
		t.Errorf("plan: %s", detail)
	}
}
