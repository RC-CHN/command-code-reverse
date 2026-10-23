package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errUpstreamIdle = errors.New("upstream idle timeout")

// chatIdle watches only upstream I/O: connection setup and body reads. Pausing
// between reads prevents slow downstream writes from looking like upstream
// inactivity. A single timer is reused without buffering or a reader goroutine.
type chatIdle struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	idle     time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	waiting  bool
	closed   bool
	deadline time.Time
}

func newChatIdle(parent context.Context, idle time.Duration) *chatIdle {
	ctx, cancel := context.WithCancelCause(parent)
	return &chatIdle{ctx: ctx, cancel: cancel, idle: idle}
}

func (w *chatIdle) begin() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctx.Err() != nil {
		return
	}
	w.waiting = true
	w.deadline = time.Now().Add(w.idle)
	if w.timer == nil {
		w.timer = time.AfterFunc(w.idle, w.expire)
	} else {
		w.timer.Reset(w.idle)
	}
}

func (w *chatIdle) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.waiting || w.ctx.Err() != nil {
		return
	}
	// Stop/Reset cannot retract an AfterFunc callback already scheduled. An
	// old callback must recheck the current read's deadline before canceling.
	if remaining := time.Until(w.deadline); remaining > 0 {
		w.timer.Reset(remaining)
		return
	}
	w.waiting = false
	w.cancel(errUpstreamIdle)
}

func (w *chatIdle) end(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waiting = false
	if w.timer != nil {
		w.timer.Stop()
	}
	if cause := context.Cause(w.ctx); cause != nil {
		return cause
	}
	return err
}

func (w *chatIdle) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.waiting = false
	if w.timer != nil {
		w.timer.Stop()
	}
	w.cancel(context.Canceled)
}

type chatIdleReader struct {
	reader io.Reader
	watch  *chatIdle
}

func (r *chatIdleReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.watch.begin()
	defer func() { err = r.watch.end(err) }()
	for {
		if cause := context.Cause(r.watch.ctx); cause != nil {
			return 0, cause
		}
		n, err = r.reader.Read(p)
		if n != 0 || err != nil {
			return n, err
		}
		// (0, nil) does not represent network activity and must not renew the
		// deadline. Any actual bytes, including heartbeat fragments, do.
	}
}
