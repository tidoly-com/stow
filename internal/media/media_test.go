package media

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/media/mediatest"
)

var seoul = time.FixedZone("KST", 9*3600)

func mustRead(t *testing.T, data []byte) Info {
	t.Helper()
	info, err := read(bytes.NewReader(data), seoul)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestCameraPhoto(t *testing.T) {
	info := mustRead(t, mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300))
	if info.Kind != "jpeg" || info.Width != 400 || info.Height != 300 || info.Orientation != 6 {
		t.Errorf("shape: %+v", info)
	}
	if info.Make != "samsung" || info.Model != "SM-S938N" || info.Lens != "Galaxy S25+ Rear Camera" {
		t.Errorf("camera: %q %q %q", info.Make, info.Model, info.Lens)
	}
	if want := time.Date(2026, 9, 25, 6, 13, 18, 0, time.UTC); !info.Taken.Equal(want) || !info.HasZone || info.Zone != 9*3600 {
		t.Errorf("taken: %v zone %d (stated: %v), want %v +9h", info.Taken, info.Zone, info.HasZone, want)
	}
	if want := time.Date(2026, 9, 25, 15, 13, 18, 0, time.UTC); !info.Wall.Equal(want) {
		t.Errorf("wall clock: %v, want %v", info.Wall, want)
	}
	if !info.HasGPS || math.Abs(info.Lat-37.5665) > 1e-6 || math.Abs(info.Lon-126.978) > 1e-6 {
		t.Errorf("gps: %v %v %v", info.HasGPS, info.Lat, info.Lon)
	}
	if info.ISO != 50 || math.Abs(info.ExposureTime-1.0/120) > 1e-9 || info.FNumber != 1.8 || info.FocalLength != 6.3 {
		t.Errorf("exposure: iso %d, %v s, f/%v, %v mm", info.ISO, info.ExposureTime, info.FNumber, info.FocalLength)
	}
}

func TestSouthWestAndNoOffset(t *testing.T) {
	x := mediatest.Camera
	x.Offset, x.LatRef, x.LonRef = "", "S", "W"
	info := mustRead(t, mediatest.JPEG(x.Bytes(), 10, 10))
	if info.Lat > 0 || info.Lon > 0 {
		t.Errorf("gps: %v %v, want both negative", info.Lat, info.Lon)
	}
	// no offset in the file: the wall clock is read as this machine's zone, and the file is not said to state it
	if want := time.Date(2026, 9, 25, 6, 13, 18, 0, time.UTC); !info.Taken.Equal(want) || info.HasZone {
		t.Errorf("taken: %v (zone stated: %v), want %v and not stated", info.Taken, info.HasZone, want)
	}
}

func TestPhotoThatSaysNothing(t *testing.T) {
	for name, data := range map[string][]byte{
		"screenshot without exif": mediatest.JPEG(nil, 1080, 2340),
		"location tag off":        mediatest.JPEG(mediatest.EXIF{Taken: "2026:09:25 15:13:18", Make: "samsung", Model: "SM-S938N"}.Bytes(), 10, 10),
		"gps written as 0,0":      mediatest.JPEG(mediatest.EXIF{LatRef: "N", LonRef: "E"}.Bytes(), 10, 10),
	} {
		info := mustRead(t, data)
		if info.HasGPS {
			t.Errorf("%s: reports a location", name)
		}
	}
	if info := mustRead(t, mediatest.JPEG(nil, 1080, 2340)); info.Width != 1080 || info.Height != 2340 || !info.Taken.IsZero() {
		t.Errorf("screenshot: %+v", info)
	}
}

func TestUnknownFormatIsNotAnError(t *testing.T) {
	info, err := read(bytes.NewReader([]byte("GIF89a and then some more bytes")), seoul)
	if err != nil || info.Kind != "" {
		t.Errorf("gif: %+v %v", info, err)
	}
}

func TestBrokenFilesFailWithoutBringingTheProgramDown(t *testing.T) {
	good := mediatest.JPEG(mediatest.Camera.Bytes(), 400, 300)
	if _, err := read(bytes.NewReader(good[:100]), seoul); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a JPEG cut off inside its headers: %v, want ErrCorrupt", err)
	}
	if _, err := read(bytes.NewReader(append([]byte{0xFF, 0xD8}, make([]byte, 10)...)), seoul); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a JPEG start and then nothing: %v, want ErrCorrupt", err)
	}
	// scribble over every part of a good file, and over container headers: any answer but a crash is fine
	r := rand.New(rand.NewSource(1))
	heads := [][]byte{good, append([]byte("\x00\x00\x00\x18ftypheic"), make([]byte, 400)...), append([]byte("\x00\x00\x00\x18ftypisom"), make([]byte, 400)...),
		append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 200)...)}
	for i := 0; i < 20000; i++ {
		data := bytes.Clone(heads[i%len(heads)])
		for n := 1 + r.Intn(8); n > 0; n-- {
			data[2+r.Intn(len(data)-2)] = byte(r.Intn(256))
		}
		read(bytes.NewReader(data), seoul)
	}
}

// Videos are checked against files ffmpeg writes, when ffmpeg is there to write them.
func TestVideosWrittenByFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	make := func(name string, args ...string) string {
		path := filepath.Join(dir, name)
		full := append([]string{"-v", "error", "-y"}, append(args, path)...)
		if out, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v %s", err, out)
		}
		return path
	}
	source := []string{"-f", "lavfi", "-i", "testsrc=d=2:s=640x360:r=30", "-pix_fmt", "yuv420p", "-metadata", "creation_time=2026-09-25T06:13:18Z"}
	plain := make("plain.mp4", append(source, "-metadata", "location=+37.5665+126.9780/")...)
	turned := make("turned.mp4", "-display_rotation", "270", "-i", plain, "-c", "copy", "-metadata", "creation_time=2026-09-25T06:13:18Z")
	android := make("android.mov", "-i", plain, "-c", "copy", "-metadata", "creation_time=2026-09-25T06:13:18Z", "-metadata", "location=+35.1796+129.0756/")
	apple := make("apple.mov", append(source, "-movflags", "use_metadata_tags", "-metadata", "com.apple.quicktime.location.ISO6709=-33.8568+151.2153+010.000/",
		"-metadata", "com.apple.quicktime.creationdate=2026-09-25T16:13:18+1000", "-metadata", "com.apple.quicktime.model=iPhone 15")...)

	// The moving half of an iPhone's live photo does not begin by saying what it is: "wide", then the
	// picture data, with the description at the end. It is a video all the same.
	whole, readErr := os.ReadFile(plain)
	if readErr != nil {
		t.Fatal(readErr)
	}
	said := int(whole[0])<<24 | int(whole[1])<<16 | int(whole[2])<<8 | int(whole[3]) // the length of the box that says what the file is
	unsaid := filepath.Join(dir, "live.mov")
	os.WriteFile(unsaid, append([]byte("\x00\x00\x00\x08wide"), whole[said:]...), 0o644)
	if info, err := Read(unsaid, time.UTC); err != nil || info.Kind != "video" || info.Width != 640 || info.Height != 360 || info.DurationMs < 1900 {
		t.Errorf("a movie that begins with \"wide\": %+v, %v — want a video of 640x360, about two seconds", info, err)
	}
	other := filepath.Join(dir, "not-a-movie.bin")
	os.WriteFile(other, []byte("\x00\x00\x00\x08free and nothing like a movie after it"), 0o644)
	if info, err := Read(other, time.UTC); err != nil || info.Kind != "" {
		t.Errorf("a file that only begins like a movie: %+v, %v — want it passed over without an error", info, err)
	}

	taken := time.Date(2026, 9, 25, 6, 13, 18, 0, time.UTC)
	for _, c := range []struct {
		path        string
		orientation int
		lat, lon    float64
		wallHour    int
		zoneStated  bool
		model       string
	}{
		{plain, 1, 37.5665, 126.978, 15, false, ""},
		{turned, 6, 37.5665, 126.978, 15, false, ""}, // ffmpeg carries the location over from its input
		{android, 1, 35.1796, 129.0756, 15, false, ""},
		{apple, 1, -33.8568, 151.2153, 16, true, "iPhone 15"},
	} {
		info, err := Read(c.path, seoul)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(c.path), err)
		}
		name := filepath.Base(c.path)
		if info.Kind != "video" || info.Width != 640 || info.Height != 360 || info.DurationMs != 2000 || info.FPS != 30 || info.Orientation != c.orientation {
			t.Errorf("%s shape: %+v", name, info)
		}
		if !info.Taken.Equal(taken) || info.Wall.Hour() != c.wallHour || info.HasZone != c.zoneStated || info.Model != c.model {
			t.Errorf("%s: taken %v, wall %v, zone stated %v, model %q", name, info.Taken, info.Wall, info.HasZone, info.Model)
		}
		if info.HasGPS != (c.lat != 0) || math.Abs(info.Lat-c.lat) > 1e-4 || math.Abs(info.Lon-c.lon) > 1e-4 {
			t.Errorf("%s gps: %v %v %v", name, info.HasGPS, info.Lat, info.Lon)
		}
	}
	// a video cut off before its index is a broken file, not an empty one
	whole, _ = os.ReadFile(plain)
	if _, err := read(bytes.NewReader(whole[:len(whole)/3]), seoul); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a third of a video: %v, want ErrCorrupt", err)
	}
}
