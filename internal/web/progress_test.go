package web

import (
	"testing"

	"github.com/tidoly-com/stow/internal/store"
)

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

// What is left counts what waits and what is going on, of the work a person sees; the full pictures made
// when someone zooms in are not part of putting the photos in order.
func TestJobsLeftCountsWhatWaitsAndWhatIsGoingOn(t *testing.T) {
	left, now := jobsLeft([]store.JobReport{
		{Kind: "face", State: "queued", Count: 2432}, {Kind: "face", State: "running", Count: 48},
		{Kind: "place", State: "running", Count: 21}, {Kind: "place", State: "done", Count: 8630},
		{Kind: "coupon", State: "queued", Count: 1282}, {Kind: "thumbnail", State: "failed", Count: 3},
		{Kind: "full", State: "queued", Count: 1},
	})
	if left != 2432+48+21+1282 || now != "장소 이름 붙이기 · 얼굴 찾기" {
		t.Errorf("%d, %q", left, now)
	}
	if left, now := jobsLeft([]store.JobReport{{Kind: "face", State: "done", Count: 10}}); left != 0 || now != "" {
		t.Errorf("all done: %d, %q", left, now)
	}
}
