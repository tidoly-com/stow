// Package coupons tells a coupon from the other pictures one keeps: a picture with a barcode that reads
// the same twice — or once, when its words say until when it can be used. The barcode is read here, on
// every system. The words come from the coupon helper, a separate program (helpers/coupons-mac, macOS
// only); without it a coupon's last day is not known. On the operator's 2,370 pictures without camera
// information this keeps 70: the 30 whose expiry the words show, and 40 more read twice — coupons whose
// date is cut off, among membership cards, QR codes and statements, which the operator would rather take
// out of the album than miss (trials/2026-10-14-coupons, trials/2026-10-16-embeddinggemma2). Without the
// helper, 68: two of the 30 read only once.
//
//	-> {"picture": "/path/to/picture"}
//	<- {"lines": ["...", ...]}     or {"error": "..."}
package coupons

import (
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/datamatrix"
	"github.com/makiuchi-d/gozxing/oned"
	"github.com/makiuchi-d/gozxing/qrcode"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/tidoly-com/stow/internal/helper"
)

type Barcode struct {
	Format string `json:"format"`
	Text   string `json:"text"`
}

type Helper struct{ helper.Process }

// New is a coupon helper started by the given command.
func New(command []string) *Helper {
	return &Helper{helper.Process{Name: "coupon helper", Command: command}}
}

// Words asks the helper for the words in a picture, line by line.
func (h *Helper) Words(ctx context.Context, picture string) ([]string, error) {
	var reply struct {
		Lines []string `json:"lines"`
	}
	err := h.Ask(ctx, map[string]string{"picture": picture}, &reply)
	return reply.Lines, err
}

// Find reads the barcode of the first of the files that opens as a picture; code.Text is "" when there is
// none. twice says the picture at three quarters of its size reads the same: patterns that are no barcode
// read as one now and then (UPC-E most of all), and seldom twice the same.
func Find(paths ...string) (code Barcode, twice bool, err error) {
	var img image.Image
	for _, p := range paths {
		f, err2 := os.Open(p)
		if err2 != nil {
			err = err2
			continue
		}
		img, _, err = image.Decode(f)
		f.Close()
		if err == nil {
			break
		}
	}
	if img == nil {
		return Barcode{}, false, err
	}
	code, ok := read(img)
	if !ok {
		return Barcode{}, false, nil
	}
	b := img.Bounds()
	small := image.NewRGBA(image.Rect(0, 0, b.Dx()*3/4, b.Dy()*3/4))
	draw.CatmullRom.Scale(small, small.Bounds(), img, b, draw.Src, nil)
	again, ok := read(small)
	return code, ok && again == code, nil
}

func read(img image.Image) (Barcode, bool) {
	bmp, err := gozxing.NewBinaryBitmap(gozxing.NewHybridBinarizer(gozxing.NewLuminanceSourceFromImage(img)))
	if err != nil {
		return Barcode{}, false
	}
	hard := map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true}
	readers := []gozxing.Reader{qrcode.NewQRCodeReader(), oned.NewCode128Reader(), oned.NewMultiFormatUPCEANReader(hard),
		oned.NewITFReader(), oned.NewCode39Reader(), datamatrix.NewDataMatrixReader()}
	for _, r := range readers {
		if res, err := r.Decode(bmp, hard); err == nil {
			return Barcode{Format: res.GetBarcodeFormat().String(), Text: res.GetText()}, true
		}
	}
	return Barcode{}, false
}

// A date as coupons write it: 2022.09.20, 2022-9-20, 2013년 2월 12일, 20181109.
const date = `(?:(20\d{2})\s*[./년-]\s*(\d{1,2})\s*[./월-]\s*(\d{1,2})|(20\d{2})(\d{2})(\d{2})(?:\D|$))`

var expiryRules = []*regexp.Regexp{
	// a word for it, then the date — or a span, whose end counts
	regexp.MustCompile(`(?i)(?:유효\s*기간|사용\s*기한|사용\s*기간|교환\s*기한|유효\s*기한|만료일|expir\w*|valid\w*)[^0-9]{0,20}(?:` + date + `\s*[~-]\s*)?` + date),
	// the date, then 까지 or "N일 남음"
	regexp.MustCompile(date + `\s*(?:\d{1,2}:\d{2}\s*)?(?:까지|\|?\s*\d+\s*일\s*남음)`),
	// the end of a span written "~ date"
	regexp.MustCompile(`~\s*` + date),
}

// Expiry finds the last day a coupon can be used in its words.
func Expiry(lines []string) (time.Time, bool) {
	text := strings.Join(lines, " ")
	for _, rule := range expiryRules {
		m := rule.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		var parts []int // the last date in the match: year, month, day
		for _, g := range m[1:] {
			if n, err := strconv.Atoi(g); err == nil {
				parts = append(parts, n)
			}
		}
		parts = parts[len(parts)-3:]
		day := time.Date(parts[0], time.Month(parts[1]), parts[2], 0, 0, 0, 0, time.UTC)
		if day.Year() == parts[0] && int(day.Month()) == parts[1] && day.Day() == parts[2] {
			return day, true
		}
	}
	return time.Time{}, false
}
