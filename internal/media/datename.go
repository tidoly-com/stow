package media

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A photo that carries no date of capture — a screenshot, a picture saved from a messenger — still often
// says when it is from: in its name (Screenshot_20231215-093011.png, KakaoTalk_20230501_123456.jpg,
// IMG_20230501_123456.jpg), or in the folder it was put in (an iPhone's export to a PC files photos under
// DCIM/201805__/; people file them under 2018-05 or 2018/05). The file's own time is no help: a copy made
// years later carries the day of the copy (seen: 217 such photos all shown on 29 August 2026, 2026-10-18).

// a day in a file's name, with the time of day when it follows
var dayInName = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d\d)[-_.]?(0[1-9]|1[0-2])[-_.]?(0[1-9]|[12]\d|3[01])(?:[-_ T.]?([01]\d|2[0-3])[-_.:]?([0-5]\d)[-_.:]?([0-5]\d))?(?:[^0-9]|$)`)

// a month in a folder's name, alone or followed by the day: 201805__, 2018-05, 2018_05_03, 20180503
var monthInFolder = regexp.MustCompile(`^((?:19|20)\d\d)[-_.]?(0[1-9]|1[0-2])(?:[-_.]?(0[1-9]|[12]\d|3[01]))?(?:[^0-9].*)?$`)

var (
	yearFolder  = regexp.MustCompile(`^((?:19|20)\d\d)$`)
	monthFolder = regexp.MustCompile(`^(0?[1-9]|1[0-2])(?:[^0-9].*)?$`)
)

// DateFromPath is the moment a photo's name or folders tell, as the clock on the wall in loc: the file's
// name first (to the second when it says so), then its folders from the nearest up (the first of the month
// at noon when only the month is known). ok is false when nothing in the path is a date, or the date lies
// in the future.
func DateFromPath(rel string, loc *time.Location) (t time.Time, ok bool) {
	rel = path.Clean(strings.ReplaceAll(rel, `\`, "/"))
	dir, name := path.Split(rel)
	name = strings.TrimSuffix(name, path.Ext(name))
	if m := dayInName.FindStringSubmatch(name); m != nil {
		hour, minute, second := 12, 0, 0
		if m[4] != "" {
			hour, minute, second = atoi(m[4]), atoi(m[5]), atoi(m[6])
		}
		if t, ok := day(atoi(m[1]), atoi(m[2]), atoi(m[3]), hour, minute, second, loc); ok {
			return t, true
		}
	}
	folders := strings.Split(strings.Trim(dir, "/"), "/")
	for i := len(folders) - 1; i >= 0; i-- {
		f := folders[i]
		if m := monthInFolder.FindStringSubmatch(f); m != nil {
			d := 1
			if m[3] != "" {
				d = atoi(m[3])
			}
			if t, ok := day(atoi(m[1]), atoi(m[2]), d, 12, 0, 0, loc); ok {
				return t, true
			}
		}
		if i > 0 { // 2018/05: a month folder inside a year folder
			if mm, yy := monthFolder.FindStringSubmatch(f), yearFolder.FindStringSubmatch(folders[i-1]); mm != nil && yy != nil {
				if t, ok := day(atoi(yy[1]), atoi(mm[1]), 1, 12, 0, 0, loc); ok {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}

func day(y, m, d, hour, minute, second int, loc *time.Location) (time.Time, bool) {
	t := time.Date(y, time.Month(m), d, hour, minute, second, 0, loc)
	if t.Month() != time.Month(m) || t.Day() != d || t.After(time.Now().Add(24*time.Hour)) { // no 31 June, no future
		return time.Time{}, false
	}
	return t, true
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
