package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Handler func(context.Context, Job) error

type Runner struct {
	Store       *Store
	RetryPolicy RetryPolicy
	Lease       time.Duration
	Poll        time.Duration
	Logger      *slog.Logger

	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewRunner(store *Store) *Runner {
	return &Runner{
		Store: store, Lease: 2 * time.Minute, Poll: time.Second,
		Logger: slog.Default(), handlers: make(map[string]Handler),
	}
}

func (r *Runner) Register(jobType string, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[jobType] = handler
}

func (r *Runner) Run(ctx context.Context) error {
	if r.Store == nil {
		return errors.New("job store is required")
	}
	poll := r.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for {
		worked, err := r.runOne(ctx)
		if err != nil {
			r.logger().Error("job runner iteration failed", "error", err)
		}
		if worked {
			continue
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (r *Runner) runOne(ctx context.Context) (bool, error) {
	lease := r.Lease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	job, err := r.Store.Claim(ctx, lease)
	if err != nil || job == nil {
		return false, err
	}
	r.mu.RLock()
	handler := r.handlers[job.Type]
	r.mu.RUnlock()
	if handler == nil {
		return true, r.Store.Fail(ctx, job.ID, "no handler registered for "+job.Type, time.Now())
	}
	handlerCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go r.renewLease(handlerCtx, job.ID, lease, heartbeatDone)
	err = handler(handlerCtx, *job)
	cancel()
	<-heartbeatDone
	if err != nil {
		delay := r.RetryPolicy.Delay(job.Attempts)
		if failErr := r.Store.Fail(ctx, job.ID, err.Error(), time.Now().Add(delay)); failErr != nil {
			return true, fmt.Errorf("job failed: %v; persist failure: %w", err, failErr)
		}
		return true, nil
	}
	return true, r.Store.Complete(ctx, job.ID)
}

func (r *Runner) renewLease(ctx context.Context, jobID string, lease time.Duration, done chan<- struct{}) {
	defer close(done)
	interval := lease / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Store.RenewLease(ctx, jobID, lease); err != nil && !errors.Is(err, context.Canceled) {
				r.logger().Warn("job lease renewal failed", "job_id", jobID, "error", err)
			}
		}
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}
