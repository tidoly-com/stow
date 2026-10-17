// Package media reads what a photo or video says about itself: when and where it was taken, with what,
// which way up, how large, how long. It reads the file formats directly — JPEG, PNG, HEIC, MP4/MOV — so
// that nothing has to be shipped next to the program for this.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Info is what was found. Zero values mean "the file does not say".
type Info struct {
	Kind string // "jpeg", "png", "heic", "video", or "" for a format this package does not read

	// Taken is the moment of capture. Wall is the clock on the wall where it was taken (stored as if UTC);
	// Zone is the offset between the two in seconds, known only if the file says so (HasZone).
	Taken   time.Time
	Wall    time.Time
	Zone    int
	HasZone bool

	HasGPS   bool
	Lat, Lon float64

	Make, Model, Lens string
	Orientation       int // EXIF orientation 1-8; 0 = not stated
	Width, Height     int // as stored, before Orientation is applied
	DurationMs        int64
	FPS               float64

	FNumber, ExposureTime, FocalLength float64
	ISO                                int
}

// ErrCorrupt means the file claims a format but cannot be read as one.
var ErrCorrupt = errors.New("corrupt media file")

// Read looks into the file at path. A format it does not know is not an error: Info.Kind is empty.
// local is the time zone to assume for a capture time that comes without one.
func Read(path string, local *time.Location) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	return read(f, local)
}

func read(f io.ReadSeeker, local *time.Location) (info Info, err error) {
	defer func() { // the parsers index into untrusted bytes; a slip must fail the file, not the program
		if r := recover(); r != nil {
			info, err = Info{}, fmt.Errorf("%w: %v", ErrCorrupt, r)
		}
	}()
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return Info{}, nil // too short to be any format this package reads
	}
	f.Seek(0, io.SeekStart)
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		info.Kind = "jpeg"
		err = readJPEG(f, &info)
	case bytes.Equal(head[:8], []byte("\x89PNG\r\n\x1a\n")):
		info.Kind = "png"
		err = readPNG(f, &info)
	case string(head[4:8]) == "ftyp":
		err = readISO(f, string(head[8:12]), &info)
	case quickTimeStart[string(head[4:8])]:
		// A QuickTime movie need not say what it is up front: the moving half of an iPhone's live photo
		// begins with "wide" and the picture data, and describes itself at the end.
		if readISO(f, "qt  ", &info) != nil {
			return Info{}, nil // those four letters, but no movie behind them
		}
	default:
		return Info{}, nil
	}
	if err != nil {
		return Info{}, err
	}
	info.settleTime(local)
	return info, nil
}

// quickTimeStart lists the boxes a QuickTime movie may begin with when it has no "ftyp" box.
var quickTimeStart = map[string]bool{"wide": true, "mdat": true, "moov": true, "free": true, "skip": true}

// settleTime fills in whichever of Taken and Wall the file did not give.
func (i *Info) settleTime(local *time.Location) {
	switch {
	case !i.Wall.IsZero() && i.HasZone: // EXIF with an offset
		i.Taken = i.Wall.Add(-time.Duration(i.Zone) * time.Second)
	case !i.Wall.IsZero(): // EXIF without an offset: assume it was taken where this machine is
		at := time.Date(i.Wall.Year(), i.Wall.Month(), i.Wall.Day(), i.Wall.Hour(), i.Wall.Minute(), i.Wall.Second(), i.Wall.Nanosecond(), local)
		i.Taken = at.UTC()
		_, i.Zone = at.Zone()
	case !i.Taken.IsZero(): // video: the container holds UTC
		at := i.Taken.In(local)
		if i.HasZone {
			at = i.Taken.In(time.FixedZone("", i.Zone))
		} else {
			_, i.Zone = at.Zone()
		}
		i.Wall = time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.UTC)
	}
}

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }
func be32(b []byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
func be64(b []byte) int64 {
	return int64(be32(b))<<32 | int64(uint32(be32(b[4:])))
}

func readAt(f io.ReadSeeker, offset int64, n int) ([]byte, error) {
	if n < 0 || n > 4<<20 {
		return nil, fmt.Errorf("%w: unreasonable length %d", ErrCorrupt, n)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("%w: file ends early", ErrCorrupt)
	}
	return buf, nil
}

// readJPEG walks the segments up to the image data: EXIF sits in an APP1 segment, the size in a SOF one.
func readJPEG(f io.ReadSeeker, info *Info) error {
	pos := int64(2)
	for {
		head, err := readAt(f, pos, 4)
		if err != nil {
			return err
		}
		if head[0] != 0xFF {
			return fmt.Errorf("%w: lost the JPEG segment chain", ErrCorrupt)
		}
		marker, length := head[1], be16(head[2:])
		if marker == 0xDA || marker == 0xD9 { // image data starts, or the file ends
			return nil
		}
		if length < 2 {
			return fmt.Errorf("%w: bad JPEG segment length", ErrCorrupt)
		}
		switch {
		case marker == 0xE1 && length >= 8:
			body, err := readAt(f, pos+4, length-2)
			if err != nil {
				return err
			}
			if bytes.HasPrefix(body, []byte("Exif\x00\x00")) {
				parseEXIF(body[6:], info) // broken EXIF does not make the photo unreadable
			}
		case marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC:
			body, err := readAt(f, pos+4, 5)
			if err != nil {
				return err
			}
			info.Height, info.Width = be16(body[1:]), be16(body[3:])
		}
		pos += 2 + int64(length)
	}
}

// readPNG takes the size from IHDR and EXIF from an eXIf chunk, if the file has one.
func readPNG(f io.ReadSeeker, info *Info) error {
	pos := int64(8)
	for {
		head, err := readAt(f, pos, 8)
		if err != nil {
			if info.Width > 0 {
				return nil // no IEND, but we have what we came for
			}
			return err
		}
		length, kind := be32(head), string(head[4:])
		switch kind {
		case "IHDR":
			body, err := readAt(f, pos+8, 8)
			if err != nil {
				return err
			}
			info.Width, info.Height = be32(body), be32(body[4:])
		case "eXIf":
			body, err := readAt(f, pos+8, length)
			if err != nil {
				return err
			}
			parseEXIF(body, info)
		case "IEND":
			return nil
		}
		pos += 12 + int64(uint32(length))
	}
}
