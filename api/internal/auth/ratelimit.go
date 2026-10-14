package auth

import (
	"sync"
	"time"
)

// limiter is a fixed-window counter per key.
// ponytail: in-process limiter, fine for one API instance; move to Postgres or Redis when the API runs on more than one machine
type limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	windows map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(maxPerWindow int, w time.Duration) *limiter {
	return &limiter{max: maxPerWindow, window: w, windows: map[string]*window{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) > 10000 { // keep the map bounded: drop finished windows
		for k, w := range l.windows {
			if now.Sub(w.start) >= l.window {
				delete(l.windows, k)
			}
		}
	}
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= l.window {
		l.windows[key] = &window{start: now, n: 1}
		return true
	}
	if w.n >= l.max {
		return false
	}
	w.n++
	return true
}
