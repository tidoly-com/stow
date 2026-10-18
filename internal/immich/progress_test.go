package immich

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/media/mediatest"
	"github.com/tidoly-com/stow/internal/store"
)

// A look at a folder counts its photos and videos first, so the folder's row can say how far it is —
// "2 / 3장 (66%)" — and not only a number that goes up. Looked at again, as at every start, it counts the
// same files: the ones already recorded are counted as seen, so the share is right after a restart too.
// The status page says the same of each kind of job: done in all of them, with a bar.
func TestTheLookAtAFolderAndTheJobsSayHowFarTheyAre(t *testing.T) {
	data, folder := t.TempDir(), t.TempDir()
	e := start(t, data, newKey(t))
	ctx := context.Background()
	owner := e.browserLogin("owner@example.com", "owner")
	for i, rel := range []string{"a.jpg", "trip/b.jpg", "trip/c.mov", "notes.txt", ".thumbnails/x.jpg", ".hidden.jpg"} {
		full := filepath.Join(folder, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, mediatest.JPEG(nil, 40+2*i, 30), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id, err := e.api.AddFolder(ctx, folder, e.userID("owner@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	ended := func() *Scan {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); e.api.FolderScan(id).Running.Load(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the look at the folder did not end")
			}
		}
		return e.api.FolderScan(id)
	}
	for look := range 2 {
		scan := ended()
		if !scan.Counted.Load() || scan.Total.Load() != 3 || scan.Seen.Load() != 3 {
			t.Errorf("look %d: counted %v, %d in all, %d seen; want 3 and 3 — two photos and a video, not the text, the hidden folder or the hidden file",
				look, scan.Counted.Load(), scan.Total.Load(), scan.Seen.Load())
		}
		e.api.ScanFolders(ctx) // as at a start
	}
	ended()
	e.settle()

	_, page := e.browse("/status", owner)
	row := regexp.MustCompile(`<td>작은 그림 만들기</td><td class="progress">([^<]*)<progress max="(\d+)" value="(\d+)">`).FindStringSubmatch(page)
	if row == nil {
		t.Fatalf("the status page must show how far the small pictures are, with a bar: %s", page)
	}
	if want := fmt.Sprintf("%s / %s장 (100%%)", row[3], row[2]); row[1] != want || row[2] == "0" {
		t.Errorf("small pictures, all done: %q, want %q", row[1], want)
	}
	if !strings.Contains(page, "모두 끝났습니다.") || strings.Contains(page, "data-working") {
		t.Errorf("all done, the status page must say so and stop looking again: %s", page)
	}
	if _, admin := e.browse("/admin", owner); strings.Contains(admin, "사진을 정리하는 중입니다") {
		t.Error("all done, the admin page still says the photos are being put in order")
	}

	// work left: coupons are off, so a coupon job waits — the status and admin pages say what is left
	var first string
	e.store.AssetsSince(ctx, e.userID("owner@example.com"), 0, func(a store.Asset) error {
		first = cmp.Or(first, a.ID)
		return nil
	})
	if err := e.store.AddJob(ctx, first, "coupon"); err != nil {
		t.Fatal(err)
	}
	if _, page := e.browse("/status", owner); !strings.Contains(page, `<section id="jobs" data-working>`) || !strings.Contains(page, "남은 작업 <b>1개</b>") {
		t.Errorf("work left, the status page must say how much and keep looking: %s", page)
	}
	if _, admin := e.browse("/admin", owner); !strings.Contains(admin, "사진을 정리하는 중입니다 — 남은 작업 1개") {
		t.Errorf("work left, the admin page must say so: %s", admin)
	}
}
