package immich

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"testing"

	"github.com/tidoly-com/stow/internal/media/mediatest"
)

// A picture no small picture is made for (a GIF) is not queued again at every start: each time, its job
// told every open page of a changed photo. A photo whose small pictures went missing still is.
func TestAPictureWithNoSmallPictureIsNotQueuedAgainAtEachStart(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	token := e.login("owner@example.com", "owner")
	var moving bytes.Buffer
	gif.Encode(&moving, image.NewPaletted(image.Rect(0, 0, 40, 30), []color.Color{color.White, color.Black}), nil)
	e.upload(token, "moving.gif", moving.Bytes())
	_, photo := e.upload(token, "photo.jpg", mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300))
	e.settle()
	if n, err := e.api.QueueMissingThumbnails(context.Background()); err != nil || n != 0 {
		t.Errorf("queued %d at start (%v): the GIF has no small picture and never will", n, err)
	}
	e.api.cfg.Thumbs.Remove(photo["id"].(string))
	if n, _ := e.api.QueueMissingThumbnails(context.Background()); n != 1 {
		t.Errorf("queued %d: the photo whose small pictures went missing must be", n)
	}
}
