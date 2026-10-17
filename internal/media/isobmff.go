package media

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HEIC photos and MP4/MOV videos share one container: a tree of boxes, each a 4-byte size, a 4-byte type
// and a body. This file walks that tree for the few boxes that describe the capture.

type box struct {
	kind       string
	start, end int64 // of the body
}

// boxes lists the boxes between start and end without reading their bodies.
func boxes(f io.ReadSeeker, start, end int64) ([]box, error) {
	var out []box
	for pos := start; pos+8 <= end && len(out) < 4096; {
		head, err := readAt(f, pos, 8)
		if err != nil {
			return nil, err
		}
		size, headLen := int64(uint32(be32(head))), int64(8)
		switch size {
		case 1: // 64-bit size follows
			big, err := readAt(f, pos+8, 8)
			if err != nil {
				return nil, err
			}
			size, headLen = be64(big), 16
		case 0: // runs to the end
			size = end - pos
		}
		if size < headLen || pos+size > end {
			return nil, fmt.Errorf("%w: box %q does not fit", ErrCorrupt, head[4:])
		}
		out = append(out, box{kind: string(head[4:]), start: pos + headLen, end: pos + size})
		pos += size
	}
	return out, nil
}

func find(list []box, kind string) (box, bool) {
	for _, b := range list {
		if b.kind == kind {
			return b, true
		}
	}
	return box{}, false
}

func (b box) body(f io.ReadSeeker) ([]byte, error) { return readAt(f, b.start, int(b.end-b.start)) }

var heicBrands = map[string]bool{"heic": true, "heix": true, "hevc": true, "hevx": true, "mif1": true, "msf1": true, "avif": true}

func readISO(f io.ReadSeeker, brand string, info *Info) error {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	top, err := boxes(f, 0, end)
	if err != nil {
		return err
	}
	if heicBrands[brand] {
		info.Kind = "heic"
		meta, ok := find(top, "meta")
		if !ok {
			return fmt.Errorf("%w: HEIC without a meta box", ErrCorrupt)
		}
		return readHEIC(f, meta, info)
	}
	info.Kind = "video"
	moov, ok := find(top, "moov")
	if !ok {
		return fmt.Errorf("%w: video without a moov box", ErrCorrupt)
	}
	return readMovie(f, moov, info)
}

// ── HEIC ────────────────────────────────────────────────────────────────────────────────────────────────────

// readHEIC finds the item of type "Exif", looks up where it lies, and reads the size from the largest
// image description.
func readHEIC(f io.ReadSeeker, meta box, info *Info) error {
	inside, err := boxes(f, meta.start+4, meta.end) // 4 bytes of version and flags first
	if err != nil {
		return err
	}
	turned := 0
	defer func() {
		if turned != 0 {
			info.Orientation = turned
		}
	}()
	if iprp, ok := find(inside, "iprp"); ok {
		if props, err := boxes(f, iprp.start, iprp.end); err == nil {
			if ipco, ok := find(props, "ipco"); ok {
				described, _ := boxes(f, ipco.start, ipco.end)
				for _, p := range described {
					b, err := p.body(f)
					switch {
					case err != nil:
					case p.kind == "ispe" && len(b) >= 12:
						if w, h := be32(b[4:]), be32(b[8:]); w*h > info.Width*info.Height {
							info.Width, info.Height = w, h
						}
					case p.kind == "irot" && len(b) >= 1:
						// quarter turns anticlockwise. In this container the turn stated here is the one that
						// counts; an EXIF orientation that disagrees is ignored (see below).
						turned = [4]int{1, 8, 3, 6}[b[0]&3]
					}
				}
			}
		}
	}

	exifItem := -1
	if iinf, ok := find(inside, "iinf"); ok {
		b, err := iinf.body(f)
		if err != nil {
			return err
		}
		skip := int64(6) // version/flags, then a 16-bit count (32-bit from version 1)
		if b[0] != 0 {
			skip = 8
		}
		entries, _ := boxes(f, iinf.start+skip, iinf.end)
		for _, e := range entries {
			eb, err := e.body(f)
			if e.kind != "infe" || err != nil || len(eb) < 12 {
				continue
			}
			switch eb[0] { // version
			case 2:
				if string(eb[8:12]) == "Exif" {
					exifItem = be16(eb[4:])
				}
			case 3:
				if len(eb) >= 14 && string(eb[10:14]) == "Exif" {
					exifItem = be32(eb[4:])
				}
			}
		}
	}
	iloc, ok := find(inside, "iloc")
	if exifItem < 0 || !ok {
		return nil // a HEIC without EXIF is still a photo
	}
	b, err := iloc.body(f)
	if err != nil {
		return err
	}
	offset, length, found := locate(b, exifItem)
	if !found || length < 8 || length > 1<<20 {
		return nil
	}
	raw, err := readAt(f, offset, int(length))
	if err != nil {
		return nil
	}
	if skip := be32(raw) + 4; skip >= 4 && skip < len(raw) { // 4 bytes say how far in the TIFF block starts
		parseEXIF(raw[skip:], info)
	}
	return nil
}

// locate reads the item location table for one item stored directly in the file.
func locate(b []byte, item int) (offset, length int64, found bool) {
	version := b[0]
	offsetSize, lengthSize, baseSize, indexSize := int(b[4]>>4), int(b[4]&15), int(b[5]>>4), int(b[5]&15)
	pos := 6
	num := func(size int) int64 {
		var v int64
		for i := 0; i < size; i++ {
			v = v<<8 | int64(b[pos+i])
		}
		pos += size
		return v
	}
	count := num(2)
	if version == 2 {
		pos -= 2
		count = num(4)
	}
	for i := int64(0); i < count; i++ {
		id := num(2)
		if version == 2 {
			pos -= 2
			id = num(4)
		}
		method := int64(0)
		if version == 1 || version == 2 {
			method = num(2) & 15
		}
		num(2) // data reference
		base := num(baseSize)
		extents := num(2)
		for e := int64(0); e < extents; e++ {
			if version == 1 || version == 2 {
				num(indexSize)
			}
			extOffset, extLength := num(offsetSize), num(lengthSize)
			if int(id) == item && method == 0 && e == 0 {
				offset, length, found = base+extOffset, extLength, true
			}
		}
	}
	return
}

// ── MP4 and MOV ─────────────────────────────────────────────────────────────────────────────────────────────

const macEpoch = 2082844800 // seconds from 1904-01-01, the container's zero, to 1970-01-01

func readMovie(f io.ReadSeeker, moov box, info *Info) error {
	inside, err := boxes(f, moov.start, moov.end)
	if err != nil {
		return err
	}
	if mvhd, ok := find(inside, "mvhd"); ok {
		b, err := mvhd.body(f)
		if err != nil {
			return err
		}
		var created, scale, duration int64
		if b[0] == 1 && len(b) >= 32 {
			created, scale, duration = be64(b[4:]), int64(be32(b[20:])), be64(b[24:])
		} else if len(b) >= 20 {
			created, scale, duration = int64(uint32(be32(b[4:]))), int64(be32(b[12:])), int64(uint32(be32(b[16:])))
		}
		if created > macEpoch {
			info.Taken = time.Unix(created-macEpoch, 0).UTC()
		}
		if scale > 0 {
			info.DurationMs = duration * 1000 / scale
		}
	}
	for _, trak := range inside {
		if trak.kind == "trak" {
			readTrack(f, trak, info)
		}
	}
	if meta, ok := find(inside, "meta"); ok { // Apple: named keys with their values
		readAppleKeys(f, meta, info)
	}
	if udta, ok := find(inside, "udta"); ok {
		parts, _ := boxes(f, udta.start, udta.end)
		if xyz, ok := find(parts, "\xa9xyz"); ok { // Android: location as "+37.5665+126.9780/"
			if b, err := xyz.body(f); err == nil && len(b) > 4 {
				info.setISO6709(string(b[4:]))
			}
		}
		if loci, ok := find(parts, "loci"); ok && !info.HasGPS { // 3GPP: a named place with fixed-point coordinates
			if b, err := loci.body(f); err == nil {
				if at := bytes.IndexByte(b[min(6, len(b)):], 0) + 6 + 2; at >= 8 && at+8 <= len(b) { // past the name and one role byte
					lon, lat := float64(int32(be32(b[at:])))/65536, float64(int32(be32(b[at+4:])))/65536
					if (lat != 0 || lon != 0) && math.Abs(lat) <= 90 && math.Abs(lon) <= 180 {
						info.Lat, info.Lon, info.HasGPS = lat, lon, true
					}
				}
			}
		}
		if meta, ok := find(parts, "meta"); ok {
			readAppleKeys(f, meta, info)
		}
	}
	return nil
}

// readTrack takes size, rotation and frame rate from the video track and ignores the others.
func readTrack(f io.ReadSeeker, trak box, info *Info) {
	inside, err := boxes(f, trak.start, trak.end)
	if err != nil {
		return
	}
	mdia, ok := find(inside, "mdia")
	if !ok {
		return
	}
	media, _ := boxes(f, mdia.start, mdia.end)
	hdlr, ok := find(media, "hdlr")
	if !ok {
		return
	}
	if b, err := hdlr.body(f); err != nil || len(b) < 12 || string(b[8:12]) != "vide" {
		return
	}
	if tkhd, ok := find(inside, "tkhd"); ok {
		if b, err := tkhd.body(f); err == nil {
			at := 40 // version 0: the matrix starts here
			if b[0] == 1 {
				at = 52
			}
			if len(b) >= at+44 {
				a, bb := int32(be32(b[at:])), int32(be32(b[at+4:]))
				switch {
				case a == 0 && bb > 0:
					info.Orientation = 6 // shown turned 90° clockwise
				case a < 0:
					info.Orientation = 3
				case a == 0 && bb < 0:
					info.Orientation = 8
				default:
					info.Orientation = 1
				}
				info.Width, info.Height = be32(b[at+36:])>>16, be32(b[at+40:])>>16
			}
		}
	}
	var scale int64
	if mdhd, ok := find(media, "mdhd"); ok {
		if b, err := mdhd.body(f); err == nil {
			if b[0] == 1 && len(b) >= 32 {
				scale = int64(be32(b[20:]))
			} else if len(b) >= 20 {
				scale = int64(be32(b[12:]))
			}
		}
	}
	if minf, ok := find(media, "minf"); ok {
		parts, _ := boxes(f, minf.start, minf.end)
		if stbl, ok := find(parts, "stbl"); ok {
			tables, _ := boxes(f, stbl.start, stbl.end)
			if stts, ok := find(tables, "stts"); ok {
				if b, err := stts.body(f); err == nil && len(b) >= 8 {
					// each entry: this many frames, each lasting this long
					var frames, ticks int64
					for i, n := 8, be32(b[4:]); n > 0 && i+8 <= len(b); i, n = i+8, n-1 {
						count := int64(uint32(be32(b[i:])))
						frames += count
						ticks += count * int64(uint32(be32(b[i+4:])))
					}
					if scale > 0 && ticks > 0 {
						info.FPS = math.Round(float64(frames)*float64(scale)/float64(ticks)*100) / 100
					}
				}
			}
		}
	}
}

// readAppleKeys reads QuickTime metadata: a list of key names and, in the same order, their values.
func readAppleKeys(f io.ReadSeeker, meta box, info *Info) {
	// QuickTime writes this box bare; MP4 puts 4 bytes of version and flags first
	head, err := readAt(f, meta.start, 12)
	if err != nil {
		return
	}
	start := meta.start
	if string(head[4:8]) != "hdlr" && string(head[8:12]) == "hdlr" {
		start += 4
	}
	inside, err := boxes(f, start, meta.end)
	if err != nil {
		return
	}
	keysBox, ok1 := find(inside, "keys")
	ilst, ok2 := find(inside, "ilst")
	if !ok1 || !ok2 {
		return
	}
	names, _ := boxes(f, keysBox.start+8, keysBox.end)
	items, _ := boxes(f, ilst.start, ilst.end)
	for _, item := range items {
		index := be32([]byte(item.kind)) - 1
		if index < 0 || index >= len(names) {
			continue
		}
		name, err := names[index].body(f)
		if err != nil {
			continue
		}
		parts, _ := boxes(f, item.start, item.end)
		data, ok := find(parts, "data")
		if !ok {
			continue
		}
		b, err := data.body(f)
		if err != nil || len(b) <= 8 {
			continue
		}
		value := string(b[8:])
		switch string(name) {
		case "com.apple.quicktime.location.ISO6709":
			info.setISO6709(value)
		case "com.apple.quicktime.make":
			info.Make = value
		case "com.apple.quicktime.model":
			info.Model = value
		case "com.apple.quicktime.creationdate":
			for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
				if at, err := time.Parse(layout, value); err == nil {
					info.Taken = at.UTC()
					_, info.Zone = at.Zone()
					info.HasZone = true
					break
				}
			}
		}
	}
}

var iso6709 = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)`)

func (i *Info) setISO6709(text string) {
	m := iso6709.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return
	}
	lat, err1 := strconv.ParseFloat(m[1], 64)
	lon, err2 := strconv.ParseFloat(m[2], 64)
	if err1 == nil && err2 == nil && (lat != 0 || lon != 0) && math.Abs(lat) <= 90 && math.Abs(lon) <= 180 {
		i.Lat, i.Lon, i.HasGPS = lat, lon, true
	}
}
