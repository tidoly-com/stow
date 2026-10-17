package immich

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/tidoly-com/stow/internal/store"
)

// memoryPhotos is how many photos one memory holds at most, taken evenly across its month.
const memoryPhotos = 30

// memoryGap is how many days a year's photos rest before they make a memory again: with few earlier
// years the same photos would come back day after day (seen: one photo of October 2025, six days running).
const memoryGap = 7

// Remember makes the memories of the coming days now and then, until ctx ends.
func (s *Server) Remember(ctx context.Context) {
	for {
		if err := s.MakeMemories(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("memories: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// MakeMemories gives every person a memory for today where there is none yet: photos of the same month from
// one earlier year, a different year from one day to the next (the operator, 2026-10-14 — Immich shows the
// same day of the year instead, which is often empty). A year shown in the last week rests; with none left,
// there is no memory that day. Only today's: the app lists every memory it has, whatever day it is for
// (seen: six cards, the days before and the days made ahead) — so past days' are taken off the devices.
func (s *Server) MakeMemories(ctx context.Context, now time.Time) error {
	users, err := s.store.Users(ctx)
	if err != nil {
		return err
	}
	today := now.Format(time.DateOnly)
	retired, err := s.store.RetireMemories(ctx, today)
	if err != nil {
		return err
	}
	for _, owner := range retired {
		s.Changed(owner)
	}
	for _, u := range users {
		made, err := s.makeMemory(ctx, u.ID, now)
		if err != nil {
			return err
		}
		if made {
			s.Changed(u.ID)
		}
	}
	return nil
}

// makeMemory makes one person's memory of a day, if there is none yet and a year to show.
func (s *Server) makeMemory(ctx context.Context, ownerID string, day time.Time) (bool, error) {
	key := day.Format(time.DateOnly)
	if has, err := s.store.HasMemory(ctx, ownerID, key); err != nil || has {
		return false, err
	}
	all, err := s.store.MonthYears(ctx, ownerID, day.Format("01"), day.Year())
	if err != nil {
		return false, err
	}
	shown, err := s.store.MemoryYears(ctx, ownerID, day.AddDate(0, 0, -(memoryGap-1)).Format(time.DateOnly), key)
	if err != nil {
		return false, err
	}
	resting := map[int]bool{}
	for _, y := range shown {
		resting[y] = true
	}
	var years []int
	for _, y := range all {
		if !resting[y] {
			years = append(years, y)
		}
	}
	if len(years) == 0 {
		return false, nil
	}
	year := memoryYear(years, day)
	photos, err := s.store.MonthPhotos(ctx, ownerID, fmt.Sprintf("%d-%s", year, day.Format("01")))
	if err != nil {
		return false, err
	}
	return true, s.store.AddMemory(ctx, ownerID, key, year, spread(photos, memoryPhotos))
}

// memoryYear picks the year a day's memory is from: the years are put in an order of their own for each
// month, and the days of the month walk through it — so one day differs from the next, and every device
// gets the same year for the same day.
func memoryYear(years []int, day time.Time) int {
	order := append([]int(nil), years...)
	rand.New(rand.NewSource(int64(day.Year()*100+int(day.Month())))).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order[(day.Day()-1)%len(order)]
}

// spread keeps at most n of the list, evenly apart, first and last included.
func spread(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	out := make([]string, n)
	for i := range out {
		out[i] = list[i*(len(list)-1)/(n-1)]
	}
	return out
}

// memoryData is SyncMemoryV1/V2: shown on its day only, dated that day in the year its photos are from.
func memoryData(m store.Memory) map[string]any {
	day, _ := time.Parse(time.DateOnly, m.Day)
	then := time.Date(m.Year, day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	if then.Month() != day.Month() { // the 29th of February in a year without one
		then = time.Date(m.Year, day.Month()+1, 0, 0, 0, 0, 0, time.UTC)
	}
	return map[string]any{"id": m.ID, "ownerId": m.OwnerID, "type": "on_this_day", "data": map[string]any{"year": m.Year},
		"createdAt": m.CreatedAt, "updatedAt": m.CreatedAt, "deletedAt": nil, "isSaved": false, "seenAt": nil,
		"memoryAt": store.Timestamp(then), "showAt": m.Day + "T00:00:00.000Z", "hideAt": m.Day + "T23:59:59.999Z"}
}
