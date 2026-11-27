package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
)

func envelope(t *testing.T, typ string, id uuid.UUID, version int64, payload map[string]any) messaging.Envelope {
	t.Helper()
	env, err := messaging.NewEnvelope(context.Background(), "booking", typ, id, version, payload)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func payload(id uuid.UUID) map[string]any {
	return map[string]any{
		"reservation_id": id, "hotel_id": uuid.New(), "hotel_name": "Гранд", "room_type_id": uuid.New(),
		"room_type_name": "Стандарт", "customer_id": uuid.New(), "check_in": "2026-12-05", "check_out": "2026-12-08",
		"guests": 2, "total": map[string]any{"amount": 1350000, "currency": "RUB"},
	}
}

func TestCardFromCurrentEvent(t *testing.T) {
	id := uuid.New()
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cancelled := created.Add(time.Hour)
	p := payload(id)
	p["status"], p["created_at"], p["cancelled_at"] = "cancelled", created, cancelled
	p["reason"], p["cancelled_by_role"] = "Отменено гостем", "customer"
	c, err := CardFromEvent(envelope(t, "ReservationCancelled", id, 2, p))
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusCancelled || c.Nights != 3 || c.Version != 2 || c.Total.Amount != 1350000 {
		t.Fatalf("unexpected card: %+v", c)
	}
	if !c.CreatedAt.Equal(created) || c.CancelledAt == nil || !c.CancelledAt.Equal(cancelled) {
		t.Fatalf("timestamps: created %v, cancelled %v", c.CreatedAt, c.CancelledAt)
	}
	if c.CancelReason != "Отменено гостем" || c.CancelledByRole != "customer" {
		t.Fatalf("cancel details: %+v", c)
	}
}

// События старого формата (до полей status/created_at): статус — из типа
// события, время — из occurred_at.
func TestCardFromLegacyEvent(t *testing.T) {
	id := uuid.New()
	env := envelope(t, "ReservationConfirmed", id, 1, payload(id))
	c, err := CardFromEvent(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusConfirmed || !c.CreatedAt.Equal(env.OccurredAt) || c.CancelledAt != nil {
		t.Fatalf("unexpected card: %+v", c)
	}
	env = envelope(t, "ReservationCancelled", id, 2, payload(id))
	c, err = CardFromEvent(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusCancelled || !c.CreatedAt.IsZero() || c.CancelledAt == nil || !c.CancelledAt.Equal(env.OccurredAt) {
		t.Fatalf("unexpected legacy cancel card: %+v", c)
	}
}

// Нарушение контракта — постоянная ошибка: событие уйдёт в DLQ, а не будет
// повторяться бесконечно.
func TestCardFromBrokenEventIsPermanent(t *testing.T) {
	id := uuid.New()
	broken := map[string]func(map[string]any){
		"no customer":     func(p map[string]any) { delete(p, "customer_id") },
		"bad dates":       func(p map[string]any) { p["check_out"] = "2026-12-01" },
		"not a date":      func(p map[string]any) { p["check_in"] = "завтра" },
		"unknown status":  func(p map[string]any) { p["status"] = "pending" },
		"other aggregate": func(p map[string]any) { p["reservation_id"] = uuid.New() },
	}
	for name, spoil := range broken {
		p := payload(id)
		spoil(p)
		if _, err := CardFromEvent(envelope(t, "ReservationConfirmed", id, 1, p)); !messaging.IsPermanent(err) {
			t.Errorf("%s: want permanent error, got %v", name, err)
		}
	}
	env := envelope(t, "ReservationConfirmed", id, 1, payload(id))
	env.Payload = json.RawMessage(`{"reservation_id": 42}`)
	if _, err := CardFromEvent(env); !messaging.IsPermanent(err) {
		t.Errorf("undecodable payload: want permanent error, got %v", err)
	}
}

func TestTodayUsesHotelTimezone(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skip("no tzdata")
	}
	// 22:30 UTC 31 декабря — в Москве уже 1 января.
	s := &Service{Location: msk, Now: func() time.Time { return time.Date(2026, 12, 31, 22, 30, 0, 0, time.UTC) }}
	if got := s.today().Format(time.DateOnly); got != "2027-01-01" {
		t.Fatalf("today = %s, want 2027-01-01", got)
	}
}
