package immich

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/tidoly-com/stow/internal/store"
)

// ScreenshotAlbum is what the album screenshots gather in is called when the server makes it.
const ScreenshotAlbum = "화면 캡처"

// screenshotNames are what phones and computers call their screenshots.
var screenshotNames = []string{"screenshot", "screen shot", "screen_shot", "screencapture", "스크린샷", "캡처", "화면 캡처"}

// isScreenshot tells a screenshot from what it already knows of a picture, without opening it again:
// nothing of a camera, and a screenshot's name — or, named like any photo (an iPhone's IMG_1234.PNG), a PNG
// the shape of a phone's screen, tall and narrow. A picture saved from a message is a JPEG with no camera
// either; it is left out.
func isScreenshot(a store.Asset) bool {
	if a.Type != "IMAGE" || a.LiveOf != "" || a.Meta == nil || a.Meta.Make != "" {
		return false
	}
	name := strings.ToLower(a.OriginalName)
	for _, n := range screenshotNames {
		if strings.Contains(name, n) {
			return true
		}
	}
	w, h := a.Meta.Width, a.Meta.Height
	if strings.ToLower(filepath.Ext(name)) != ".png" || w <= 0 || h <= 0 {
		return false
	}
	long, short := max(w, h), min(w, h)
	return long*10 >= short*16 // 16:9 and longer; a tablet's 4:3 is not taken
}

// fileScreenshot puts a picture just read in its owner's screenshot album, when it is one, screenshots are
// gathered, and it was never put there before.
func (s *Server) fileScreenshot(ctx context.Context, a store.Asset) error {
	if !isScreenshot(a) || !s.store.ScreenshotsOn(ctx) {
		return nil
	}
	fresh, err := s.store.AddScreenshot(ctx, a.ID)
	if err != nil || !fresh {
		return err
	}
	album, err := s.store.AutoAlbum(ctx, a.OwnerID, "screenshots", ScreenshotAlbum)
	if err != nil {
		return err
	}
	_, err = s.store.ChangeAlbumAssets(ctx, a.OwnerID, album.ID, []string{a.ID}, true)
	return err
}

// FileScreenshots looks at every picture already read that was never put in the screenshot album — at
// start, and when screenshots are switched on — and puts the screenshots there.
func (s *Server) FileScreenshots(ctx context.Context) (filed int, err error) {
	if !s.store.ScreenshotsOn(ctx) {
		return 0, nil
	}
	list, err := s.store.ScreenshotCandidates(ctx)
	if err != nil {
		return 0, err
	}
	owners := map[string]bool{}
	for _, a := range list {
		if !isScreenshot(a) {
			continue
		}
		if err := s.fileScreenshot(ctx, a); err != nil {
			return filed, err
		}
		filed++
		owners[a.OwnerID] = true
	}
	for owner := range owners {
		s.Changed(owner) // the app answers with a sync, which brings the album
	}
	return filed, nil
}

// SetScreenshots switches gathering screenshots on or off. On, the pictures already there are looked at
// too. Off, nothing more is put in; the album stays as it is.
func (s *Server) SetScreenshots(ctx context.Context, on bool) error {
	if err := s.store.SetScreenshotsOn(ctx, on); err != nil {
		return err
	}
	_, err := s.FileScreenshots(ctx)
	return err
}

// ScreenshotsOn says whether screenshots are gathered.
func (s *Server) ScreenshotsOn(ctx context.Context) bool { return s.store.ScreenshotsOn(ctx) }
