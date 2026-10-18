// Package jobs runs the work that follows an upload — reading metadata, making thumbnails — a fixed number
// at a time, behind the requests instead of inside them. The queue is in the database, so work survives a
// restart.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/tidoly-com/stow/internal/store"
)

const maxAttempts = 3

// Handler does one kind of work for one asset.
type Handler func(ctx context.Context, assetID string) error

type Runner struct {
	store    *store.Store
	handlers map[string]Handler
	wake     chan struct{}
}

func NewRunner(s *store.Store) *Runner {
	return &Runner{store: s, handlers: map[string]Handler{}, wake: make(chan struct{}, 1)}
}

// Handle registers the work for a kind. Jobs of a kind nobody handles stay queued.
func (r *Runner) Handle(kind string, h Handler) { r.handlers[kind] = h }

// Wake tells the workers there may be something new; it never blocks.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run works the queue with the given number of workers until ctx ends.
func (r *Runner) Run(ctx context.Context, workers int) {
	if n, err := r.store.RequeueRunning(ctx); err != nil {
		log.Printf("jobs: requeue: %v", err)
	} else if n > 0 {
		log.Printf("jobs: %d left unfinished by the last run, queued again", n)
	}
	kinds := make([]string, 0, len(r.handlers))
	for kind := range r.handlers {
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		<-ctx.Done()
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.work(ctx, kinds)
		}()
	}
	wg.Wait()
}

func (r *Runner) work(ctx context.Context, kinds []string) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		job, err := r.store.ClaimJob(ctx, kinds)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, sql.ErrNoRows):
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-tick.C:
			}
			continue
		case err != nil:
			log.Printf("jobs: claim: %v", err)
			time.Sleep(time.Second)
			continue
		}
		jobErr := r.handlers[job.Kind](ctx, job.AssetID)
		if ctx.Err() != nil {
			return // stopped mid-job: it stays "running" and the next start queues it again
		}
		if jobErr != nil {
			log.Printf("jobs: %s for %s failed (attempt %d): %v", job.Kind, job.AssetID, job.Attempts, jobErr)
		}
		r.finish(ctx, job, jobErr)
		r.Wake() // let an idle worker look too
	}
}

// finish records how a job went, trying again until it is written: a job left "running" is done by no one
// until the next start, and keeps the work from ever reading as all done.
func (r *Runner) finish(ctx context.Context, job store.Job, jobErr error) {
	for wait := time.Second; ; wait = min(2*wait, time.Minute) {
		err := r.store.FinishJob(ctx, job, jobErr, maxAttempts)
		if err == nil || ctx.Err() != nil {
			return // stopped: it stays "running" and the next start queues it again
		}
		log.Printf("jobs: finish %s for %s: %v — trying again in %v", job.Kind, job.AssetID, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
