package immich

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"sort"
	"strings"
	"testing"

	"github.com/tidoly-com/stow/internal/media/mediatest"
)

func pngOf(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

// Screenshots gather in an album of their own, on from the start: by their name, or as a phone-shaped PNG
// with no camera. A camera's photo and a picture saved from a message stay out; one taken out of the album
// is not put back; switched off nothing more goes in, switched on again what came meanwhile does.
func TestScreenshotsGatherInAnAlbum(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	phone := e.login("owner@example.com", "owner")
	owner := e.userID("owner@example.com")
	ids := map[string]string{}
	put := func(name string, data []byte) {
		t.Helper()
		_, a := e.upload(phone, name, data)
		ids[name] = a["id"].(string)
	}
	put("Screenshot_20231215-093011.jpg", mediatest.JPEG(nil, 40, 80))
	put("IMG_1234.PNG", pngOf(60, 130))                 // an iPhone's screenshot
	put("IMG_1235.PNG", pngOf(80, 60))                  // a picture, not a screen
	put("KakaoTalk_1.jpg", mediatest.JPEG(nil, 40, 90)) // saved from a message: no camera, but a JPEG
	put("Screenshot_by_camera.jpg", mediatest.JPEG(mediatest.EXIF{Make: "Apple", Model: "iPhone 15"}.Bytes(), 40, 80))
	e.settle()

	album := func() []string {
		t.Helper()
		b, err := e.store.AutoAlbum(ctx, owner, "screenshots", ScreenshotAlbum)
		if err != nil {
			t.Fatal(err)
		}
		in, _, err := e.store.AlbumCoupons(ctx, owner, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, a := range in {
			names = append(names, a.OriginalName)
		}
		sort.Strings(names)
		return names
	}
	if got := strings.Join(album(), " "); got != "IMG_1234.PNG Screenshot_20231215-093011.jpg" {
		t.Fatalf("album: %s", got)
	}

	b, _ := e.store.AutoAlbum(ctx, owner, "screenshots", ScreenshotAlbum)
	if status, _ := e.call("DELETE", "/api/albums/"+b.ID+"/assets", phone, map[string]any{"ids": []string{ids["IMG_1234.PNG"]}}); status != 200 {
		t.Fatalf("take out: %d", status)
	}
	if _, err := e.api.FileScreenshots(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(album(), " "); got != "Screenshot_20231215-093011.jpg" {
		t.Errorf("taken out, then put back: %s", got)
	}

	if err := e.api.SetScreenshots(ctx, false); err != nil {
		t.Fatal(err)
	}
	put("Screenshot_2.png", pngOf(62, 130)) // not the same file as IMG_1234.PNG, which would be the same photo
	e.settle()
	if got := strings.Join(album(), " "); got != "Screenshot_20231215-093011.jpg" {
		t.Errorf("switched off, still gathered: %s", got)
	}
	if err := e.api.SetScreenshots(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(album(), " "); got != "Screenshot_2.png Screenshot_20231215-093011.jpg" {
		t.Errorf("switched on again: %s", got)
	}
}
