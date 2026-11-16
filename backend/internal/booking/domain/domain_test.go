package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
)

var today = NewDate(2026, time.December, 1)

func mustStay(t *testing.T, in, out Date) Stay {
	t.Helper()
	s, err := NewStay(in, out, today)
	if err != nil {
		t.Fatalf("NewStay: %v", err)
	}
	return s
}

func TestStayInvariants(t *testing.T) {
	cases := []struct {
		name    string
		in, out Date
		field   string
	}{
		{"past check-in", today.AddDays(-1), today.AddDays(2), "check_in"},
		{"out equals in", today.AddDays(3), today.AddDays(3), "check_out"},
		{"out before in", today.AddDays(5), today.AddDays(3), "check_out"},
		{"too long", today, today.AddDays(31), "check_out"},
		{"too far ahead", today.AddDays(366), today.AddDays(367), "check_in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewStay(c.in, c.out, today)
			e := apperr.As(err)
			if e.Kind != apperr.KindValidation || e.Fields[c.field] == "" {
				t.Fatalf("want validation error on %s, got %v", c.field, err)
			}
		})
	}
	s := mustStay(t, today, today.AddDays(30))
	if s.Nights() != 30 || len(s.Days()) != 30 {
		t.Fatalf("30 nights expected, got %d/%d", s.Nights(), len(s.Days()))
	}
	// Полуинтервал: день выезда не занят.
	if last := s.Days()[29]; last.String() != today.AddDays(29).String() {
		t.Fatalf("last night must be the day before check-out, got %s", last)
	}
}

func offer() RoomOffer {
	return RoomOffer{RoomTypeID: uuid.New(), HotelID: uuid.New(), Name: "Стандарт", Capacity: 2, TotalRooms: 5,
		PricePerDay: Money{Amount: 450000, Currency: "RUB"}, Sellable: true}
}

func TestOnlyCustomerBooks(t *testing.T) {
	s := mustStay(t, today.AddDays(1), today.AddDays(3))
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleManager} {
		_, err := NewReservation(auth.Principal{UserID: uuid.New(), Role: role}, offer(), s, 1, time.Now())
		if !apperr.Is(err, apperr.KindForbidden) {
			t.Fatalf("%s must not book, got %v", role, err)
		}
	}
	r, err := NewReservation(auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer}, offer(), s, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.Total.Amount != 2*450000 {
		t.Fatalf("total = nights × price, got %d", r.Total.Amount)
	}
}

func TestCapacityAndSellable(t *testing.T) {
	s := mustStay(t, today.AddDays(1), today.AddDays(2))
	c := auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer}
	if _, err := NewReservation(c, offer(), s, 3, time.Now()); !apperr.Is(err, apperr.KindValidation) {
		t.Fatalf("3 guests in a double room must fail, got %v", err)
	}
	o := offer()
	o.Sellable = false
	if _, err := NewReservation(c, o, s, 1, time.Now()); !apperr.Is(err, apperr.KindConflict) {
		t.Fatalf("inactive offer must fail, got %v", err)
	}
}

func TestCancelRules(t *testing.T) {
	customer := auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer}
	mk := func(in, out Date) Reservation {
		r, err := NewReservation(customer, offer(), Stay{CheckIn: in, CheckOut: out}, 1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := mk(today.AddDays(1), today.AddDays(3))
	if err := r.Cancel(customer, false, "", time.Now(), today); err != nil {
		t.Fatalf("customer cancels before check-in day: %v", err)
	}
	if r.Status != Cancelled || r.Version != 2 || r.CancelReason == "" {
		t.Fatalf("unexpected state after cancel: %+v", r)
	}
	if err := r.Cancel(customer, false, "", time.Now(), today); !apperr.Is(err, apperr.KindConflict) {
		t.Fatalf("double cancel must conflict, got %v", err)
	}

	r = mk(today, today.AddDays(2))
	if err := r.Cancel(customer, false, "", time.Now(), today); !apperr.Is(err, apperr.KindConflict) {
		t.Fatalf("customer cannot cancel on check-in day, got %v", err)
	}

	stranger := auth.Principal{UserID: uuid.New(), Role: auth.RoleCustomer}
	r = mk(today.AddDays(2), today.AddDays(3))
	if err := r.Cancel(stranger, false, "", time.Now(), today); !apperr.Is(err, apperr.KindNotFound) {
		t.Fatalf("other customer must get not found, got %v", err)
	}

	manager := auth.Principal{UserID: uuid.New(), Role: auth.RoleManager}
	r = mk(today, today.AddDays(2))
	if err := r.Cancel(manager, false, "no-show", time.Now(), today); !apperr.Is(err, apperr.KindNotFound) {
		t.Fatalf("manager of another hotel must get not found, got %v", err)
	}
	if err := r.Cancel(manager, true, "", time.Now(), today); !apperr.Is(err, apperr.KindValidation) {
		t.Fatalf("manager must give a reason, got %v", err)
	}
	if err := r.Cancel(manager, true, "Гость не приехал", time.Now(), today); err != nil {
		t.Fatalf("manager cancels during stay: %v", err)
	}

	system := auth.Principal{Role: auth.RoleSystem}
	r = mk(today, today.AddDays(2))
	if err := r.Cancel(system, false, "", time.Now(), today); !apperr.Is(err, apperr.KindConflict) {
		t.Fatalf("system must not cancel a started stay, got %v", err)
	}
	r = mk(today.AddDays(1), today.AddDays(2))
	if err := r.Cancel(system, false, "hotel off", time.Now(), today); err != nil || r.CancelledBy != nil {
		t.Fatalf("system cancel: err=%v by=%v", err, r.CancelledBy)
	}
}
