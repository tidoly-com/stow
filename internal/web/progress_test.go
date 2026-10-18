package web

import "testing"

func TestCountsReadAsPeopleSayThem(t *testing.T) {
	for _, c := range []struct {
		done, total int64
		want        string
	}{
		{3913, 14298, "3,913 / 14,298장 (27%)"},
		{14297, 14298, "14,297 / 14,298장 (99%)"}, // not 100 until the last one
		{14298, 14298, "14,298 / 14,298장 (100%)"},
		{0, 0, "0장"},
		{12, 0, "12장"},
		{999, 1000, "999 / 1,000장 (99%)"},
		{1234567, 1234567, "1,234,567 / 1,234,567장 (100%)"},
	} {
		if got := progressText(c.done, c.total); got != c.want {
			t.Errorf("%d of %d: %q, want %q", c.done, c.total, got, c.want)
		}
	}
}
