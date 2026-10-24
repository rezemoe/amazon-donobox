package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// A card that keeps failing before it is even submitted is parked for review after this many tries.
const maxAttempts = 5

// Worker redeems queued cards one at a time.
type Worker struct {
	store       *Store
	redeemer    Redeemer
	delay       time.Duration // pause between redemptions
	pause       time.Duration // how long to stop after a session/captcha problem or repeated failures
	maxFailures int           // consecutive rejected cards before pausing

	wake chan struct{}
	done chan struct{}

	mu                  sync.Mutex
	pausedUntil         time.Time
	pauseReason         string
	consecutiveFailures int
}

func NewWorker(store *Store, r Redeemer, delay, pause time.Duration, maxFailures int) *Worker {
	return &Worker{
		store:       store,
		redeemer:    r,
		delay:       delay,
		pause:       pause,
		maxFailures: maxFailures,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
}

// Notify wakes the worker when a new card is queued.
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Done is closed once Run has returned.
func (w *Worker) Done() <-chan struct{} { return w.done }

type WorkerStatus struct {
	Paused              bool       `json:"paused"`
	PausedUntil         *time.Time `json:"paused_until,omitempty"`
	PauseReason         string     `json:"pause_reason,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

func (w *Worker) Status() WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := WorkerStatus{ConsecutiveFailures: w.consecutiveFailures}
	if time.Now().Before(w.pausedUntil) {
		until := w.pausedUntil
		st.Paused, st.PausedUntil, st.PauseReason = true, &until, w.pauseReason
	}
	return st
}

func (w *Worker) pauseFor(d time.Duration, reason string) {
	w.mu.Lock()
	w.pausedUntil, w.pauseReason = time.Now().Add(d), reason
	w.mu.Unlock()
	log.Printf("worker paused for %s: %s", d, reason)
}

// Resume clears a pause early.
func (w *Worker) Resume() {
	w.mu.Lock()
	w.pausedUntil, w.pauseReason, w.consecutiveFailures = time.Time{}, "", 0
	w.mu.Unlock()
	w.Notify()
}

func (w *Worker) pausedRemaining() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Until(w.pausedUntil)
}

func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)
	for ctx.Err() == nil {
		if d := w.pausedRemaining(); d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			case <-w.wake: // Resume() or a new card; re-check the pause
			}
			continue
		}

		card, err := w.store.ClaimNext(ctx)
		if err != nil {
			log.Printf("claim next card: %v", err)
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}
		if card == nil {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			case <-time.After(time.Minute):
			}
			continue
		}

		w.process(card)
		if !sleep(ctx, w.delay) {
			return
		}
	}
}

// process redeems a single card. It deliberately ignores shutdown so an in-flight
// redemption finishes and its outcome is recorded.
func (w *Worker) process(card *GiftCard) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	log.Printf("redeeming %s (attempt %d)", card.ID, card.Attempts)
	msg, err := w.redeemer.Redeem(ctx, card.Code)

	var to string
	switch {
	case err == nil:
		to = StateRedeemed
		w.mu.Lock()
		w.consecutiveFailures = 0
		w.mu.Unlock()

	case errors.Is(err, ErrCardRejected):
		to, msg = StateFailed, err.Error()
		w.mu.Lock()
		w.consecutiveFailures++
		n := w.consecutiveFailures
		w.mu.Unlock()
		// Amazon locks gift card redemption on accounts with too many failed attempts.
		if n >= w.maxFailures {
			w.pauseFor(w.pause, "too many rejected cards in a row")
		}

	case errors.Is(err, ErrNotSubmitted):
		to, msg = StateQueued, err.Error()
		if card.Attempts >= maxAttempts {
			to = StateNeedsReview
		}
		w.pauseFor(w.pause, err.Error())

	default:
		// The code may have been submitted; never retry automatically.
		to, msg = StateNeedsReview, err.Error()
	}

	if _, err := w.store.Transition(context.Background(), card.ID, []string{StateRedeeming}, to, msg); err != nil {
		log.Printf("save state for %s: %v", card.ID, err)
		return
	}
	log.Printf("%s -> %s %s", card.ID, to, msg)
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
