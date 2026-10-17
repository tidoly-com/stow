// Package thumbs makes the two small pictures the apps show instead of the original: a thumbnail for
// grids and a preview for looking at one photo. They are made once, after upload, and kept under
// derived/ — a folder that can be deleted and made again.
//
// JPEG and PNG are decoded here. HEIC photos and videos need ffmpeg to get a picture out of them; without
// it they simply have no small pictures.
package thumbs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/image/draw"

	"github.com/tidoly-com/stow/internal/media"
)

const (
	ThumbnailShortSide = 256  // grids crop to squares, so the short side sets the sharpness
	PreviewLongSide    = 1440 // enough for a phone screen
	maxPixels          = 120_000_000
)

// ErrNoDecoder means the file needs ffmpeg and ffmpeg is not available.
var ErrNoDecoder = errors.New("no decoder for this kind of file")

type Maker struct {
	Dir    string // derived/
	FFmpeg string // path to ffmpeg, or empty
}

// Path is where a small picture of an asset lives; size is "thumbnail" or "preview".
func (m *Maker) Path(assetID, size string) string {
	return filepath.Join(m.Dir, assetID[:2], assetID+"-"+size+".jpg")
}

// Size reads the width and height of a picture made here.
func (m *Maker) Size(path string) (width, height int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	return cfg.Width, cfg.Height, err
}

// Crop cuts a square around a box out of a picture — a face, with some room around it — and writes it
// 250 pixels wide to dst.
func (m *Maker) Crop(src, dst string, x1, y1, x2, y2 int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	img, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	b := img.Bounds()
	side := int(float64(max(x2-x1, y2-y1)) * 1.6)
	side = min(side, b.Dx(), b.Dy())
	cx, cy := (x1+x2)/2, (y1+y2)/2
	left, top := min(max(cx-side/2, 0), b.Dx()-side), min(max(cy-side/2, 0), b.Dy()-side)
	out := image.NewRGBA(image.Rect(0, 0, 250, 250))
	draw.ApproxBiLinear.Scale(out, out.Bounds(), img, image.Rect(b.Min.X+left, b.Min.Y+top, b.Min.X+left+side, b.Min.Y+top+side), draw.Src, nil)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return write(dst, out, 85)
}

// Remove deletes whatever small pictures exist under an id — an asset's, or a person's.
func (m *Maker) Remove(id string) {
	for _, size := range []string{"thumbnail", "preview", "person"} {
		os.Remove(m.Path(id, size))
	}
}

// Has reports whether both small pictures exist.
func (m *Maker) Has(assetID string) bool {
	for _, size := range []string{"thumbnail", "preview"} {
		if _, err := os.Stat(m.Path(assetID, size)); err != nil {
			return false
		}
	}
	return true
}

// Make writes both small pictures for the file at src. info is what media.Read said about it.
// A kind of file there is no picture to take from (info.Kind == "") is not an error.
func (m *Maker) Make(ctx context.Context, src, assetID string, info media.Info) error {
	var full image.Image
	var err error
	orientation := info.Orientation
	switch info.Kind {
	case "":
		return nil
	case "jpeg", "png":
		if info.Width*info.Height > maxPixels {
			return fmt.Errorf("picture of %dx%d is too large to decode", info.Width, info.Height)
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		full, _, err = image.Decode(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("decode: %w", err)
		}
	default: // heic, video: ffmpeg hands over a picture that is already the right way up
		at := time.Duration(0)
		if info.Kind == "video" {
			at = min(time.Second, time.Duration(info.DurationMs)*time.Millisecond/2) // the very first frame is often black
		}
		if full, err = m.frame(ctx, src, at); err != nil {
			return err
		}
		orientation = 1
	}
	if err := os.MkdirAll(filepath.Dir(m.Path(assetID, "preview")), 0o755); err != nil {
		return err
	}
	b := full.Bounds()
	long, short := max(b.Dx(), b.Dy()), min(b.Dx(), b.Dy())
	preview := upright(scale(full, float64(PreviewLongSide)/float64(long)), orientation)
	if err := write(m.Path(assetID, "preview"), preview, 82); err != nil {
		return err
	}
	// the thumbnail is cut from the preview: far fewer pixels to go through a second time
	pb := preview.Bounds()
	ratio := float64(ThumbnailShortSide) / float64(min(pb.Dx(), pb.Dy()))
	if short <= ThumbnailShortSide {
		ratio = 1
	}
	return write(m.Path(assetID, "thumbnail"), scale(preview, ratio), 78)
}

// frame asks ffmpeg for one picture from a HEIC photo or a video.
func (m *Maker) frame(ctx context.Context, src string, at time.Duration) (image.Image, error) {
	if m.FFmpeg == "" {
		return nil, ErrNoDecoder
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := []string{"-v", "error", "-nostdin"}
	if at > 0 {
		args = append(args, "-ss", strconv.FormatFloat(at.Seconds(), 'f', 3, 64))
	}
	args = append(args, "-i", src, "-frames:v", "1", "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "2", "pipe:1")
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, m.FFmpeg, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %.200s", err, errOut.String())
	}
	img, err := jpeg.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg gave no picture: %w", err)
	}
	return img, nil
}

// scale returns the picture resized by ratio; a ratio of 1 or more leaves the size alone.
func scale(src image.Image, ratio float64) image.Image {
	b := src.Bounds()
	if ratio >= 1 {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(b.Dx())*ratio+0.5)), max(1, int(float64(b.Dy())*ratio+0.5))))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

// upright turns and mirrors a picture stored with an EXIF orientation so that it shows the right way up.
func upright(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch orientation {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func write(path string, img image.Image, quality int) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = jpeg.Encode(f, img, &jpeg.Options{Quality: quality})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path) // a reader never sees half a picture
}
