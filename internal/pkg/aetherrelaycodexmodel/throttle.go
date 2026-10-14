package codexmodel

import (
	"sync"
	"time"
)

// WarningLimiter affects logs only; every call retains its model observation.
type WarningLimiter struct {
	mu   sync.Mutex
	last map[[3]string]time.Time
}

func (l *WarningLimiter) Allow(accountID, requested, served string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := [3]string{accountID, normalize(requested), normalize(served)}
	if at, ok := l.last[key]; ok && now.Sub(at) < 10*time.Minute {
		return false
	}
	if len(l.last) >= 1024 {
		for k, at := range l.last {
			if now.Sub(at) >= 10*time.Minute {
				delete(l.last, k)
			}
		}
		if len(l.last) >= 1024 {
			clear(l.last)
		}
	}
	if l.last == nil {
		l.last = make(map[[3]string]time.Time)
	}
	l.last[key] = now
	return true
}
