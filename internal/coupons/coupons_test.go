package coupons

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"
)

// The ways the operator's coupons write it (trials/2026-10-14-coupons), and what is not an expiry.
func TestExpiry(t *testing.T) {
	for _, c := range []struct {
		lines []string
		want  string
	}{
		{[]string{"아메리카노 T", "유효기간", "2023.05.09"}, "2023-05-09"},
		{[]string{"교환기한 : 2024년 1월 3일"}, "2024-01-03"},
		{[]string{"사용기간 2023.07.03 ~ 2023.08.01"}, "2023-08-01"},
		{[]string{"쿠폰", "1회결제", "2023.07.03 ~ 2023.08.01"}, "2023-08-01"},
		{[]string{"* 기간 : ~ 20181109 까지"}, "2018-11-09"},
		{[]string{"~ 2022.09.20", "22일 남음"}, "2022-09-20"},
		{[]string{"Valid until 2025-12-31"}, "2025-12-31"},
		{[]string{"주문일시 2023년 02월 19일 오후 08시 19분"}, ""},
		{[]string{"고객센터 1588-0000"}, ""},
		{[]string{"유효기간 2023.02.30"}, ""}, // no such day
	} {
		got, ok := Expiry(c.lines)
		if s := got.Format(time.DateOnly); ok != (c.want != "") || ok && s != c.want {
			t.Errorf("%q: %v %v, want %q", c.lines, s, ok, c.want)
		}
	}
}

// A barcode is read, from the first file that opens as a picture; a picture without one is no coupon.
func TestFind(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, img image.Image) string {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		os.WriteFile(p, b.Bytes(), 0o644)
		return p
	}
	code, err := oned.NewCode128Writer().Encode("123456789012", gozxing.BarcodeFormat_CODE_128, 600, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	coupon := write("coupon.jpg", code)
	blank := write("blank.jpg", image.NewGray(image.Rect(0, 0, 600, 200)))
	heic := filepath.Join(dir, "IMG_1.HEIC") // what Go cannot open: the preview is read instead
	os.WriteFile(heic, []byte("not a picture Go reads"), 0o644)

	if got, twice, err := Find(coupon); err != nil || !twice || got.Text != "123456789012" || got.Format != "CODE_128" {
		t.Errorf("a barcode: %+v %v %v", got, twice, err)
	}
	if got, twice, err := Find(heic, coupon); err != nil || !twice || got.Text != "123456789012" {
		t.Errorf("through the preview: %+v %v %v", got, twice, err)
	}
	if got, _, err := Find(blank); err != nil || got.Text != "" {
		t.Errorf("no barcode: %+v %v", got, err)
	}
	if got, _, err := Find(heic); err == nil || got.Text != "" {
		t.Errorf("nothing that opens: %+v %v", got, err)
	}
}
