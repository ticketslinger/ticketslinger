// Package ratelimit counts requests per key in fixed windows.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter per key.
// ponytail: in-process limiter, fine for one API instance; move to Postgres or Redis when the API runs on more than one machine
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	windows map[string]*window
}

type window struct {
	start time.Time
	n     int
}

// New allows maxPerWindow requests per key in each window.
func New(maxPerWindow int, w time.Duration) *Limiter {
	return &Limiter{max: maxPerWindow, window: w, windows: map[string]*window{}}
}

// Allow counts one request for key and reports whether it fits in the window.
func (l *Limiter) Allow(key string, now time.Time) bool {
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
