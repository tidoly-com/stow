package media

import (
	"encoding/binary"
	"strings"
	"time"
)

// EXIF is a small TIFF file: directories of 12-byte entries, each a tag, a type, a count and either the
// value or the offset of the value. Only the tags below are read.
const (
	tagMake         = 0x010F
	tagModel        = 0x0110
	tagOrientation  = 0x0112
	tagModified     = 0x0132 // DateTime: when the file was last written
	tagExifIFD      = 0x8769
	tagGPSIFD       = 0x8825
	tagExposureTime = 0x829A
	tagFNumber      = 0x829D
	tagISO          = 0x8827
	tagTaken        = 0x9003 // DateTimeOriginal
	tagDigitized    = 0x9004
	tagOffsetTaken  = 0x9011 // OffsetTimeOriginal
	tagFocalLength  = 0x920A
	tagLensModel    = 0xA434

	gpsLatRef = 1
	gpsLat    = 2
	gpsLonRef = 3
	gpsLon    = 4
)

type tiffEntry struct {
	kind  int
	count int
	value []byte // the 4 value bytes, or the bytes they point to
}

type tiffFile struct {
	b     []byte
	order binary.ByteOrder
}

var tiffTypeSize = map[int]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}

// dir reads the directory at offset. Anything that points outside the data is skipped.
func (t tiffFile) dir(offset int) map[int]tiffEntry {
	entries := map[int]tiffEntry{}
	if offset <= 0 || offset+2 > len(t.b) {
		return entries
	}
	n := int(t.order.Uint16(t.b[offset:]))
	for i := 0; i < n && i < 512; i++ {
		at := offset + 2 + 12*i
		if at+12 > len(t.b) {
			break
		}
		e := tiffEntry{kind: int(t.order.Uint16(t.b[at+2:])), count: int(t.order.Uint32(t.b[at+4:]))}
		size := tiffTypeSize[e.kind] * e.count
		if size <= 0 || e.count < 0 || e.count > 1<<16 {
			continue
		}
		if size <= 4 {
			e.value = t.b[at+8 : at+8+size]
		} else {
			start := int(t.order.Uint32(t.b[at+8:]))
			if start < 0 || start+size > len(t.b) {
				continue
			}
			e.value = t.b[start : start+size]
		}
		entries[int(t.order.Uint16(t.b[at:]))] = e
	}
	return entries
}

func (t tiffFile) text(e tiffEntry) string {
	if e.kind != 2 {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(e.value), "\x00 "))
}

func (t tiffFile) integer(e tiffEntry) int {
	switch {
	case e.kind == 3 && len(e.value) >= 2:
		return int(t.order.Uint16(e.value))
	case (e.kind == 4 || e.kind == 9) && len(e.value) >= 4:
		return int(t.order.Uint32(e.value))
	}
	return 0
}

// rationals reads fractions (numerator/denominator pairs).
func (t tiffFile) rationals(e tiffEntry) []float64 {
	if e.kind != 5 && e.kind != 10 {
		return nil
	}
	out := make([]float64, 0, e.count)
	for i := 0; i+8 <= len(e.value); i += 8 {
		num, den := float64(t.order.Uint32(e.value[i:])), float64(t.order.Uint32(e.value[i+4:]))
		if e.kind == 10 {
			num, den = float64(int32(t.order.Uint32(e.value[i:]))), float64(int32(t.order.Uint32(e.value[i+4:])))
		}
		if den == 0 {
			out = append(out, 0)
			continue
		}
		out = append(out, num/den)
	}
	return out
}

func (t tiffFile) rational(e tiffEntry) float64 {
	if r := t.rationals(e); len(r) > 0 {
		return r[0]
	}
	return 0
}

// parseEXIF fills info from a TIFF block. Damaged EXIF leaves info as far as it got.
func parseEXIF(b []byte, info *Info) {
	if len(b) < 8 {
		return
	}
	t := tiffFile{b: b}
	switch string(b[:2]) {
	case "II":
		t.order = binary.LittleEndian
	case "MM":
		t.order = binary.BigEndian
	default:
		return
	}
	main := t.dir(int(t.order.Uint32(b[4:])))
	info.Make, info.Model = t.text(main[tagMake]), t.text(main[tagModel])
	if o := t.integer(main[tagOrientation]); o >= 1 && o <= 8 {
		info.Orientation = o
	}

	exif := t.dir(t.integer(main[tagExifIFD]))
	stamp := t.text(exif[tagTaken])
	if stamp == "" {
		stamp = t.text(exif[tagDigitized])
	}
	if stamp == "" { // some phones write only this one
		stamp = t.text(main[tagModified])
	}
	if wall, err := time.Parse("2006:01:02 15:04:05", stamp); err == nil {
		info.Wall = wall
		if at, err := time.Parse("-07:00", t.text(exif[tagOffsetTaken])); err == nil {
			_, info.Zone = at.Zone()
			info.HasZone = true
		}
	}
	info.ExposureTime, info.FNumber = t.rational(exif[tagExposureTime]), t.rational(exif[tagFNumber])
	info.FocalLength, info.ISO, info.Lens = t.rational(exif[tagFocalLength]), t.integer(exif[tagISO]), t.text(exif[tagLensModel])

	gps := t.dir(t.integer(main[tagGPSIFD]))
	lat, lon := t.rationals(gps[gpsLat]), t.rationals(gps[gpsLon])
	if len(lat) == 3 && len(lon) == 3 {
		info.Lat = lat[0] + lat[1]/60 + lat[2]/3600
		info.Lon = lon[0] + lon[1]/60 + lon[2]/3600
		if t.text(gps[gpsLatRef]) == "S" {
			info.Lat = -info.Lat
		}
		if t.text(gps[gpsLonRef]) == "W" {
			info.Lon = -info.Lon
		}
		// cameras without a fix write 0,0 — that is the sea off Africa, not a place this photo was taken
		info.HasGPS = (info.Lat != 0 || info.Lon != 0) && info.Lat >= -90 && info.Lat <= 90 && info.Lon >= -180 && info.Lon <= 180
	}
}
