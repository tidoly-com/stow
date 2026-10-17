package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/store"
)

func open(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "stow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addAssets(t *testing.T, st *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	u, err := st.UpsertUser(ctx, "sub", "a@example.com", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		a := store.Asset{ID: store.NewID(), OwnerID: u.ID, Checksum: []byte{byte(i)}, OriginalName: "x", Path: store.NewID(), Type: "IMAGE"}
		if err := st.AddAsset(ctx, &a, []string{"metadata"}); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, st *store.Store, state string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if counts, _ := st.JobCounts(context.Background()); counts[state] == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	counts, _ := st.JobCounts(context.Background())
	t.Fatalf("jobs: %v, want %d %s", counts, n, state)
}

func TestWorkLeftByAStoppedRunIsFinishedByTheNext(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir)
	addAssets(t, st, 6)

	// first run: stopped while two jobs are in the middle
	ctx, stop := context.WithCancel(context.Background())
	started := make(chan struct{}, 6)
	r := NewRunner(st)
	r.Handle("metadata", func(ctx context.Context, _ string) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan struct{})
	go func() { r.Run(ctx, 2); close(done) }()
	<-started
	<-started
	stop()
	<-done
	if counts, _ := st.JobCounts(context.Background()); counts["running"] != 2 || counts["queued"] != 4 {
		t.Fatalf("after the stop: %v, want 2 running and 4 queued", counts)
	}
	st.Close()

	// second run over the same database
	st = open(t, dir)
	var ran int32
	r = NewRunner(st)
	r.Handle("metadata", func(context.Context, string) error { atomic.AddInt32(&ran, 1); return nil })
	ctx, stop = context.WithCancel(context.Background())
	defer stop()
	go r.Run(ctx, 2)
	waitFor(t, st, "done", 6)
	if ran != 6 {
		t.Fatalf("handler ran %d times, want 6", ran)
	}
}

func TestNeverMoreThanTheGivenNumberAtOnce(t *testing.T) {
	st := open(t, t.TempDir())
	addAssets(t, st, 20)
	var mu sync.Mutex
	now, most := 0, 0
	r := NewRunner(st)
	r.Handle("metadata", func(context.Context, string) error {
		mu.Lock()
		now++
		most = max(most, now)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		now--
		mu.Unlock()
		return nil
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go r.Run(ctx, 3)
	waitFor(t, st, "done", 20)
	if most != 3 {
		t.Fatalf("most jobs at once: %d, want 3", most)
	}
}

func TestAFailingJobIsTriedThreeTimesThenSetAside(t *testing.T) {
	st := open(t, t.TempDir())
	addAssets(t, st, 2)
	var tries int32
	r := NewRunner(st)
	r.Handle("metadata", func(context.Context, string) error { atomic.AddInt32(&tries, 1); return errors.New("unreadable") })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go r.Run(ctx, 1)
	waitFor(t, st, "failed", 2)
	if tries != 6 {
		t.Fatalf("tries: %d, want 6", tries)
	}
}

func TestJobsNobodyHandlesStayQueued(t *testing.T) {
	st := open(t, t.TempDir())
	addAssets(t, st, 3)
	r := NewRunner(st)
	r.Handle("thumbnail", func(context.Context, string) error { return nil })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go r.Run(ctx, 2)
	time.Sleep(200 * time.Millisecond)
	if counts, _ := st.JobCounts(context.Background()); counts["queued"] != 3 {
		t.Fatalf("jobs: %v, want 3 queued", counts)
	}
}
