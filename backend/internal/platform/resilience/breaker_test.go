package resilience

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBreakerStateMachine(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := NewBreaker(3, 10*time.Second)
	b.Now = c.now

	for i := 0; i < 2; i++ {
		if err := b.Allow(); err != nil {
			t.Fatal(err)
		}
		b.Done(false)
	}
	if b.State() != Closed {
		t.Fatal("2 failures < threshold: still closed")
	}
	_ = b.Allow()
	b.Done(true) // успех сбрасывает счётчик подряд идущих отказов
	for i := 0; i < 3; i++ {
		_ = b.Allow()
		b.Done(false)
	}
	if b.State() != Open {
		t.Fatalf("3 consecutive failures → open, got %s", b.State())
	}
	if err := b.Allow(); err != ErrOpen {
		t.Fatal("open breaker must fail fast")
	}
	c.advance(10 * time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("after timeout one probe passes: %v", err)
	}
	if err := b.Allow(); err != ErrOpen {
		t.Fatal("only one probe in half-open")
	}
	b.Done(false)
	if b.State() != Open {
		t.Fatal("failed probe → open again")
	}
	c.advance(10 * time.Second)
	_ = b.Allow()
	b.Done(true)
	if b.State() != Closed {
		t.Fatalf("successful probe → closed, got %s", b.State())
	}
}

func TestTokenBucket(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	tb := NewTokenBucket(1, 3)
	tb.Now = c.now
	for i := 0; i < 3; i++ {
		if !tb.Allow("ip") {
			t.Fatalf("burst of 3, request %d rejected", i)
		}
	}
	if tb.Allow("ip") {
		t.Fatal("4th request in the same instant must be rejected")
	}
	if !tb.Allow("other-ip") {
		t.Fatal("limits are per key")
	}
	c.advance(time.Second)
	if !tb.Allow("ip") {
		t.Fatal("one token refilled after 1s")
	}
}
