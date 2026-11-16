// Package resilience — circuit breaker (из ПКП-4) и token bucket.
package resilience

import (
	"errors"
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	default:
		return "half_open"
	}
}

var ErrOpen = errors.New("circuit breaker is open")

// Breaker — конечный автомат closed → open → half_open → closed.
//
//   - closed: считаем подряд идущие отказы; при FailureThreshold → open.
//   - open: вызовы сразу отклоняются ErrOpen (fail fast), пока не истечёт
//     OpenTimeout.
//   - half_open: пропускаем не более HalfOpenMax пробных вызовов; первый
//     отказ → снова open, HalfOpenMax успехов → closed.
type Breaker struct {
	FailureThreshold int
	OpenTimeout      time.Duration
	HalfOpenMax      int
	Now              func() time.Time
	OnChange         func(from, to State)

	mu        sync.Mutex
	state     State
	failures  int
	openedAt  time.Time
	inFlight  int
	successes int
}

func NewBreaker(threshold int, openTimeout time.Duration) *Breaker {
	return &Breaker{FailureThreshold: threshold, OpenTimeout: openTimeout, HalfOpenMax: 1}
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Breaker) setState(s State) {
	if b.state == s {
		return
	}
	from := b.state
	b.state = s
	b.failures, b.inFlight, b.successes = 0, 0, 0
	if s == Open {
		b.openedAt = b.now()
	}
	if b.OnChange != nil {
		b.OnChange(from, s)
	}
}

// Allow решает, можно ли выполнить вызов. После вызова обязательно Done.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.OpenTimeout {
			return ErrOpen
		}
		b.setState(HalfOpen)
		fallthrough
	case HalfOpen:
		max := b.HalfOpenMax
		if max <= 0 {
			max = 1
		}
		if b.inFlight >= max {
			return ErrOpen
		}
		b.inFlight++
	}
	return nil
}

// Done сообщает результат вызова.
func (b *Breaker) Done(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		if success {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.FailureThreshold {
			b.setState(Open)
		}
	case HalfOpen:
		b.inFlight--
		if !success {
			b.setState(Open)
			return
		}
		b.successes++
		max := b.HalfOpenMax
		if max <= 0 {
			max = 1
		}
		if b.successes >= max {
			b.setState(Closed)
		}
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) >= b.OpenTimeout {
		return HalfOpen
	}
	return b.state
}

// TokenBucket — ограничитель частоты по ключу (IP, пользователь).
type TokenBucket struct {
	Rate  float64 // токенов в секунду
	Burst float64
	Now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewTokenBucket(rate, burst float64) *TokenBucket {
	return &TokenBucket{Rate: rate, Burst: burst, buckets: map[string]*bucket{}}
}

func (t *TokenBucket) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Allow списывает один токен; false — лимит исчерпан.
func (t *TokenBucket) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.lastGC) > time.Minute {
		for k, b := range t.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(t.buckets, k)
			}
		}
		t.lastGC = now
	}
	b, ok := t.buckets[key]
	if !ok {
		b = &bucket{tokens: t.Burst, last: now}
		t.buckets[key] = b
	}
	b.tokens = minF(t.Burst, b.tokens+now.Sub(b.last).Seconds()*t.Rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
