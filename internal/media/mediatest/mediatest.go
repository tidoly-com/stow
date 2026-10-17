// Package mediatest makes small media files for tests.
package mediatest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math"
)

// EXIF describes a little-endian TIFF block the way a phone camera writes one: a main directory pointing
// to an EXIF and a GPS directory. Empty fields are left out of the block.
type EXIF struct {
	Taken, Offset, Make, Model, Lens string
	Orientation, ISO                 int
	Lat, Lon                         [3]float64 // degrees, minutes, seconds
	LatRef, LonRef                   string
	Exposure, FNumber, Focal         [2]uint32
}

// Bytes renders the block.
func (x EXIF) Bytes() []byte {
	type entry struct {
		tag, kind int
		data      []byte
		count     int
	}
	ascii := func(tag int, s string) entry { return entry{tag, 2, append([]byte(s), 0), len(s) + 1} }
	short := func(tag, v int) entry { return entry{tag, 3, binary.LittleEndian.AppendUint16(nil, uint16(v)), 1} }
	rat := func(tag int, pairs ...[2]uint32) entry {
		var b []byte
		for _, p := range pairs {
			b = binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(b, p[0]), p[1])
		}
		return entry{tag, 5, b, len(pairs)}
	}
	dms := func(v [3]float64) [][2]uint32 {
		return [][2]uint32{{uint32(v[0]), 1}, {uint32(v[1]), 1}, {uint32(math.Round(v[2] * 10000)), 10000}}
	}
	var mainDir, exifDir, gpsDir []entry
	if x.Make != "" {
		mainDir = append(mainDir, ascii(0x010F, x.Make), ascii(0x0110, x.Model))
	}
	if x.Orientation != 0 {
		mainDir = append(mainDir, short(0x0112, x.Orientation))
	}
	if x.Taken != "" {
		exifDir = append(exifDir, ascii(0x9003, x.Taken))
	}
	if x.Offset != "" {
		exifDir = append(exifDir, ascii(0x9011, x.Offset))
	}
	if x.ISO != 0 {
		exifDir = append(exifDir, short(0x8827, x.ISO), rat(0x829A, x.Exposure), rat(0x829D, x.FNumber), rat(0x920A, x.Focal), ascii(0xA434, x.Lens))
	}
	if x.LatRef != "" {
		gpsDir = append(gpsDir, ascii(1, x.LatRef), rat(2, dms(x.Lat)...), ascii(3, x.LonRef), rat(4, dms(x.Lon)...))
	}

	out := []byte("II*\x00\x08\x00\x00\x00")
	// directories are laid out one after another; values that do not fit in 4 bytes follow all of them
	sizes := []int{2 + 12*(len(mainDir)+2) + 4, 2 + 12*len(exifDir) + 4, 2 + 12*len(gpsDir) + 4}
	exifAt, gpsAt := 8+sizes[0], 8+sizes[0]+sizes[1]
	heap := gpsAt + sizes[2]
	long := func(tag, v int) entry { return entry{tag, 4, binary.LittleEndian.AppendUint32(nil, uint32(v)), 1} }
	mainDir = append(mainDir, long(0x8769, exifAt), long(0x8825, gpsAt))
	var tail []byte
	for _, dir := range [][]entry{mainDir, exifDir, gpsDir} {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(dir)))
		for _, e := range dir {
			out = binary.LittleEndian.AppendUint16(out, uint16(e.tag))
			out = binary.LittleEndian.AppendUint16(out, uint16(e.kind))
			out = binary.LittleEndian.AppendUint32(out, uint32(e.count))
			if len(e.data) <= 4 {
				out = append(out, append(e.data, 0, 0, 0, 0)[:4]...)
			} else {
				out = binary.LittleEndian.AppendUint32(out, uint32(heap+len(tail)))
				tail = append(tail, e.data...)
			}
		}
		out = append(out, 0, 0, 0, 0)
	}
	return append(out, tail...)
}

// JPEG makes a real picture of the given size — a diagonal gradient, so that turning it can be told from
// not turning it — with the EXIF block, if any, placed where a camera puts it.
func JPEG(exif []byte, width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(255 * x / width), uint8(255 * y / height), 128, 255})
		}
	}
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	out := []byte{0xFF, 0xD8}
	if exif != nil {
		body := append([]byte("Exif\x00\x00"), exif...)
		out = append(out, 0xFF, 0xE1, byte((len(body)+2)>>8), byte(len(body)+2))
		out = append(out, body...)
	}
	return append(out, picture.Bytes()[2:]...)
}

// Camera is a photo as a phone would describe it: Seoul, 25 September 2026, 15:13:18 local time, held upright.
var Camera = EXIF{Taken: "2026:09:25 15:13:18", Offset: "+09:00", Make: "samsung", Model: "SM-S938N", Lens: "Galaxy S25+ Rear Camera",
	Orientation: 6, ISO: 50, Lat: [3]float64{37, 33, 59.4}, LatRef: "N", Lon: [3]float64{126, 58, 40.8}, LonRef: "E",
	Exposure: [2]uint32{1, 120}, FNumber: [2]uint32{18, 10}, Focal: [2]uint32{63, 10}}
