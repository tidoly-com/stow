package immich

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/media/mediatest"
	"github.com/tidoly-com/stow/internal/store"
)

// A photo that carries no date of capture is filed on the day its name or its folders tell — an iPhone's
// export folder DCIM/201805__, Screenshot_20231215-093011 — not the day the file was copied. Folders are
// read for one's own folder only: a photo taken in lies in this server's own year/month folders.
func TestUndatedPhotosTakeTheDayTheirNameOrFolderTells(t *testing.T) {
	data, folder := t.TempDir(), t.TempDir()
	e := start(t, data, newKey(t))
	ctx := context.Background()
	phone := e.login("owner@example.com", "owner")
	for rel, w := range map[string]int{"DCIM/201805__/IMG_6122.JPG": 40, "Screenshot_20231215-093011.jpg": 42, "DCIM/IMG_0001.JPG": 44} {
		full := filepath.Join(folder, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, mediatest.JPEG(nil, w, 30), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id, err := e.api.AddFolder(ctx, folder, e.userID("owner@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); e.api.FolderScan(id).Running.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the look at the folder did not end")
		}
	}
	_, named := e.upload(phone, "Screenshot_20190102-030405.jpg", mediatest.JPEG(nil, 46, 30))
	_, plain := e.upload(phone, "plain.jpg", mediatest.JPEG(nil, 48, 30))
	e.settle()

	day := func(a store.Asset) string { // the day the pages and the app file it under
		if a.Meta != nil && a.Meta.LocalTime != "" {
			return a.Meta.LocalTime[:10]
		}
		return a.FileCreatedAt[:10]
	}
	byName := map[string]store.Asset{}
	e.store.AssetsSince(ctx, e.userID("owner@example.com"), 0, func(a store.Asset) error {
		byName[a.Path] = a
		return nil
	})
	if got := day(byName["DCIM/201805__/IMG_6122.JPG"]); got != "2018-05-01" {
		t.Errorf("export folder: %s", got)
	}
	if got := day(byName["Screenshot_20231215-093011.jpg"]); got != "2023-12-15" {
		t.Errorf("screenshot name: %s", got)
	}
	if got, today := day(byName["DCIM/IMG_0001.JPG"]), time.Now().Format("2006-01-02"); got != today { // nothing to tell: the file's own day
		t.Errorf("no date anywhere: %s, want the file's day %s", got, today)
	}
	for asset, want := range map[string]string{named["id"].(string): "2019-01-02", plain["id"].(string): "2026-09-25"} {
		a, _ := e.store.AssetByID(ctx, asset)
		if got := day(a); got != want {
			t.Errorf("taken in %s: %s, want %s", a.OriginalName, got, want)
		}
	}
}
