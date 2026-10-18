package media

import (
	"testing"
	"time"
)

// The day a photo's name or folders tell, when the photo itself carries none.
func TestDateFromPath(t *testing.T) {
	seoul := time.FixedZone("KST", 9*3600)
	for path, want := range map[string]string{
		"아이폰사진/DCIM/201805__/IMG_6122.PNG":           "2018-05-01 12:00:00", // an iPhone's export to a PC
		"DCIM/201612__/IMG_3473.PNG":                 "2016-12-01 12:00:00",
		"Screenshots/Screenshot_20231215-093011.png": "2023-12-15 09:30:11", // Android
		"KakaoTalk_20230501_123456789.jpg":           "2023-05-01 12:00:00", // the digits run on: the day only
		"KakaoTalk_Photo_2026-10-17-15-57-55.jpeg":   "2026-10-17 15:57:55",
		"Camera/IMG_20230501_081500.jpg":             "2023-05-01 08:15:00",
		"사진/2019-07/바다.jpg":                          "2019-07-01 12:00:00",
		"사진/2019/07/바다.jpg":                          "2019-07-01 12:00:00",
		"사진/2019_07_03 여행/바다.jpg":                    "2019-07-03 12:00:00",
		"사진/2019/7월 가족/바다.jpg":                       "2019-07-01 12:00:00",
		"사진/20190703/IMG_0001.JPG":                   "2019-07-03 12:00:00",
		"2018-04/Screenshot_2019-01-02-03-04-05.png": "2019-01-02 03:04:05", // the name before the folder
	} {
		got, ok := DateFromPath(path, seoul)
		if !ok || got.Format("2006-01-02 15:04:05") != want || got.Location() != seoul {
			t.Errorf("%s: %v %v, want %s", path, got, ok, want)
		}
	}
	for _, path := range []string{
		"DCIM/IMG_5709.JPG",          // a counter, not a date
		"DCIM/100APPLE/IMG_0001.JPG", // 100APPLE is no month
		"photo_12345678.jpg",
		"2019-13-01.jpg", "2019-02-30.jpg", // no such day
		"Screenshot_20991231-000000.png", // the future
		"영수증/IMG_0001.jpg",
	} {
		if got, ok := DateFromPath(path, seoul); ok {
			t.Errorf("%s: %v, want none", path, got)
		}
	}
}
