package immich

import (
	"context"
	"encoding/json"
	"image/jpeg"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/media/mediatest"
)

// Zooming into a HEIC photo asks for a full-size JPEG: made once by the job runner, kept, and sent only
// where the original may go — a shared link that keeps originals back does not give its pixels either.
func TestHEICZoomIsMadeOnceAndOnlyWhereTheOriginalMayGo(t *testing.T) {
	if ffmpegPath == "" {
		t.Skip("ffmpeg not installed")
	}
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	phone := e.login("owner@example.com", "owner")
	// ffmpeg reads what is in the file, not its name: a JPEG named .HEIC stands in for an iPhone's photo
	_, h := e.upload(phone, "IMG_0001.HEIC", mediatest.JPEG(nil, 400, 300))
	_, j := e.upload(phone, "a.jpg", mediatest.JPEG(nil, 40, 30))
	e.settle()
	hID, jID := h["id"].(string), j["id"].(string)
	cookie := e.browserLogin("owner@example.com", "owner")
	if _, page := e.browse("/web/photo/"+hID, cookie); !strings.Contains(page, `data-full="/api/assets/`+hID+`/full"`) {
		t.Errorf("the HEIC photo's page does not zoom into the full picture: %s", page)
	}
	if _, page := e.browse("/web/photo/"+jID, cookie); !strings.Contains(page, `data-full="/api/assets/`+jID+`/original"`) {
		t.Errorf("the JPEG's page does not zoom into the original: %s", page)
	}

	full := func(id, query string) int {
		res := e.do("GET", "/api/assets/"+id+"/full"+query, phone, nil, "")
		res.Body.Close()
		return res.StatusCode
	}
	if got := full(hID, ""); got != 202 {
		t.Fatalf("first zoom: %d, want 202 while it is made", got)
	}
	if got := full(hID, ""); got != 202 && got != 200 {
		t.Fatalf("asked again while it is made: %d", got)
	}
	before := e.settle()
	if before["done"] != 2*2+1 { // metadata and thumbnail for each photo, and one full picture however often it was asked for
		t.Errorf("jobs: %v", before)
	}
	res := e.do("GET", "/api/assets/"+hID+"/full", phone, nil, "")
	cfg, err := jpeg.DecodeConfig(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || err != nil || cfg.Width != 400 || cfg.Height != 300 {
		t.Fatalf("made: %d %v %dx%d", res.StatusCode, err, cfg.Width, cfg.Height)
	}
	made, err := os.Stat(e.api.cfg.Thumbs.Path(hID, "full"))
	if err != nil {
		t.Fatal(err)
	}
	full(hID, "")
	if after := e.settle(); after["done"] != before["done"] {
		t.Errorf("made again: %v then %v", before, after)
	}
	if again, _ := os.Stat(e.api.cfg.Thumbs.Path(hID, "full")); !again.ModTime().Equal(made.ModTime()) {
		t.Error("the full picture was written again")
	}
	if got := full(jID, ""); got != 404 {
		t.Errorf("a JPEG, which browsers show as it is: %d", got)
	}

	// through a shared link: as the original goes, so does the full picture
	if err := e.api.setLink(ctx, Link{Slug: testSlug, Host: testSlug + ".stow.tidoly.app", Key: "tdly_stow_secret"}); err != nil {
		t.Fatal(err)
	}
	status, raw := e.call("POST", "/api/shared-links", phone, map[string]any{"type": "INDIVIDUAL", "assetIds": []string{hID}})
	var link map[string]any
	json.Unmarshal(raw, &link)
	key, _ := link["key"].(string)
	if status != 201 || key == "" {
		t.Fatalf("create: %d %s", status, raw)
	}
	byKey := func() int {
		res, _ := e.browse("/api/assets/" + hID + "/full?key=" + key)
		return res.StatusCode
	}
	if got := byKey(); got != 200 {
		t.Errorf("a link that gives originals: %d", got)
	}
	if status, raw := e.call("PATCH", "/api/shared-links/"+link["id"].(string), phone, map[string]any{"allowDownload": false}); status != 200 {
		t.Fatalf("edit: %d %s", status, raw)
	}
	if got := byKey(); got != 404 {
		t.Errorf("a link that keeps originals back: %d", got)
	}
}

// Before its small pictures are made, a photo's thumbnail stands in with the original — but not through a
// shared link that keeps originals back: that one gets a blank until the small picture is there.
func TestALinkThatKeepsOriginalsBackNeverSendsOneForAThumbnail(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	phone := e.login("owner@example.com", "owner")
	original := mediatest.JPEG(nil, 400, 300)
	_, a := e.upload(phone, "a.jpg", original)
	e.settle()
	id := a["id"].(string)
	for _, size := range []string{"thumbnail", "preview"} { // as before the thumbnail job has run
		os.Remove(e.api.cfg.Thumbs.Path(id, size))
	}
	if err := e.api.setLink(ctx, Link{Slug: testSlug, Host: testSlug + ".stow.tidoly.app", Key: "tdly_stow_secret"}); err != nil {
		t.Fatal(err)
	}
	status, raw := e.call("POST", "/api/shared-links", phone, map[string]any{"type": "INDIVIDUAL", "assetIds": []string{id}, "allowDownload": false})
	var link map[string]any
	json.Unmarshal(raw, &link)
	key, _ := link["key"].(string)
	if status != 201 || key == "" {
		t.Fatalf("create: %d %s", status, raw)
	}
	for _, size := range []string{"", "?size=preview"} {
		q := "?key=" + key
		if size != "" {
			q = size + "&key=" + key
		}
		res, body := e.browse("/api/assets/" + id + "/thumbnail" + q)
		if res.StatusCode != 200 || len(body) >= len(original) || strings.Contains(body, string(original[:64])) {
			t.Errorf("thumbnail%s through the link: %d, %d bytes — the original is %d", size, res.StatusCode, len(body), len(original))
		}
	}
	res := e.do("GET", "/api/assets/"+id+"/thumbnail", phone, nil, "")
	n, _ := io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if n != int64(len(original)) {
		t.Errorf("its owner, before the small picture is made: %d bytes, want the original's %d", n, len(original))
	}
}

// A photo read further — its place, its faces — names its month to the web pages, so that an open page
// fetches that month again and not every month it shows.
func TestAPhotoReadFurtherNamesItsMonthToThePages(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	phone := e.login("owner@example.com", "owner")
	_, a := e.upload(phone, "a.jpg", mediatest.JPEG(nil, 40, 30))
	e.settle()
	id, owner := a["id"].(string), e.userID("owner@example.com")
	asset, err := e.store.AssetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	events, stop := e.api.WebEvents(owner)
	defer stop()
	e.api.hub.emit(owner, "on_asset_update", map[string]any{"id": id})
	select {
	case line := <-events:
		var got struct{ Event, Month string }
		json.Unmarshal([]byte(line), &got)
		if want := assetMonth(asset); got.Event != "on_asset_update" || got.Month != want || want == "" {
			t.Errorf("%s, want the month %q", line, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
}
