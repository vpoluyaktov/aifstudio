package server

import (
	"sync"

	"golang.org/x/time/rate"
)

// aiUserLimiter provides per-user token-bucket rate limiting for AI endpoints.
// Each user gets an independent bucket seeded with the configured rate and burst.
type aiUserLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	qps     rate.Limit
	burst   int
}

func newAIUserLimiter(qps float64, burst int) *aiUserLimiter {
	return &aiUserLimiter{
		buckets: make(map[string]*rate.Limiter),
		qps:     rate.Limit(qps),
		burst:   burst,
	}
}

// Allow returns true if the user is within the rate limit.
func (l *aiUserLimiter) Allow(userUID string) bool {
	l.mu.Lock()
	lim, ok := l.buckets[userUID]
	if !ok {
		lim = rate.NewLimiter(l.qps, l.burst)
		l.buckets[userUID] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}
