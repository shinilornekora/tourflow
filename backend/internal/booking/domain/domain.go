// Package domain — бронь и правила её жизненного цикла.
//
// Агрегат Reservation с полуинтервалом проживания [check_in, check_out) —
// продолжение агрегата из ПКП-2: ночь check_out не занята, поэтому выезд
// одного гостя и заезд другого в один день не конфликтуют.
package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
)

const (
	MaxNights     = 30
	MaxDaysAhead  = 365
	MaxGuests     = 10
	DateLayout    = "2006-01-02"
	CancelReasonL = 500
)

// Date — календарная дата без времени и часового пояса.
type Date struct{ t time.Time }

func NewDate(y int, m time.Month, d int) Date { return Date{time.Date(y, m, d, 0, 0, 0, 0, time.UTC)} }

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return Date{}, err
	}
	return Date{t}, nil
}

// Today — «сегодня» в часовом поясе отелей (по умолчанию Москва).
func Today(now time.Time, loc *time.Location) Date {
	y, m, d := now.In(loc).Date()
	return NewDate(y, m, d)
}

func (d Date) Time() time.Time              { return d.t }
func (d Date) String() string               { return d.t.Format(DateLayout) }
func (d Date) AddDays(n int) Date           { return Date{d.t.AddDate(0, 0, n)} }
func (d Date) Before(o Date) bool           { return d.t.Before(o.t) }
func (d Date) After(o Date) bool            { return d.t.After(o.t) }
func (d Date) DaysUntil(o Date) int         { return int(o.t.Sub(d.t).Hours() / 24) }
func (d Date) IsZero() bool                 { return d.t.IsZero() }
func DateFromTime(t time.Time) Date         { y, m, dd := t.Date(); return NewDate(y, m, dd) }
func (d Date) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText — пара к MarshalText: сохранённый ответ команды (ПР-4)
// читается обратно тем же типом.
func (d *Date) UnmarshalText(b []byte) error {
	v, err := ParseDate(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Stay — value object «проживание»: полуинтервал дат.
type Stay struct {
	CheckIn  Date
	CheckOut Date
}

// NewStay проверяет инварианты относительно «сегодня».
func NewStay(in, out, today Date) (Stay, error) {
	fe := apperr.FieldErrors{}
	switch {
	case in.IsZero():
		fe.Add("check_in", "Укажите дату заезда")
	case in.Before(today):
		fe.Add("check_in", "Дата заезда уже прошла")
	case today.DaysUntil(in) > MaxDaysAhead:
		fe.Add("check_in", "Бронировать можно не дальше чем на год вперёд")
	}
	switch {
	case out.IsZero():
		fe.Add("check_out", "Укажите дату выезда")
	case !out.After(in):
		fe.Add("check_out", "Дата выезда должна быть позже даты заезда")
	case in.DaysUntil(out) > MaxNights:
		fe.Add("check_out", "Не больше 30 ночей в одной брони")
	}
	if err := fe.Err(); err != nil {
		return Stay{}, err
	}
	return Stay{CheckIn: in, CheckOut: out}, nil
}

func (s Stay) Nights() int { return s.CheckIn.DaysUntil(s.CheckOut) }

// Days — все занятые ночи (без дня выезда).
func (s Stay) Days() []Date {
	out := make([]Date, 0, s.Nights())
	for d := s.CheckIn; d.Before(s.CheckOut); d = d.AddDays(1) {
		out = append(out, d)
	}
	return out
}

type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type Status string

const (
	Confirmed Status = "confirmed"
	Cancelled Status = "cancelled"
)

// RoomOffer — то, что booking знает о категории номеров из реплики каталога.
type RoomOffer struct {
	RoomTypeID  uuid.UUID
	HotelID     uuid.UUID
	HotelName   string
	Name        string
	Capacity    int
	TotalRooms  int
	PricePerDay Money
	Sellable    bool // категория и отель активны
}

type Reservation struct {
	ID           uuid.UUID
	HotelID      uuid.UUID
	RoomTypeID   uuid.UUID
	CustomerID   uuid.UUID
	Stay         Stay
	Guests       int
	Status       Status
	Total        Money
	HotelName    string
	RoomTypeName string
	CreatedAt    time.Time
	CancelledAt  *time.Time
	CancelledBy  *uuid.UUID
	CancelReason string
	Version      int64
}

// NewReservation — бронирует только клиент: администратор и менеджер
// управляют отелями, но не занимают номера (требование курсовой SPA).
func NewReservation(p auth.Principal, offer RoomOffer, stay Stay, guests int, now time.Time) (Reservation, error) {
	if p.Role != auth.RoleCustomer {
		return Reservation{}, apperr.Forbidden("Бронировать номера может только клиент")
	}
	if !offer.Sellable {
		return Reservation{}, apperr.Conflict("not_sellable", "Эта категория номеров сейчас не продаётся")
	}
	if guests < 1 || guests > MaxGuests {
		return Reservation{}, apperr.Validation(map[string]string{"guests": "Гостей: от 1 до 10"})
	}
	if guests > offer.Capacity {
		return Reservation{}, apperr.Validation(map[string]string{"guests": "В этой категории помещается не больше гостей, чем указано во вместимости"})
	}
	return Reservation{
		ID: uuid.New(), HotelID: offer.HotelID, RoomTypeID: offer.RoomTypeID, CustomerID: p.UserID,
		Stay: stay, Guests: guests, Status: Confirmed,
		Total:     Money{Amount: offer.PricePerDay.Amount * int64(stay.Nights()), Currency: offer.PricePerDay.Currency},
		HotelName: offer.HotelName, RoomTypeName: offer.Name, CreatedAt: now, Version: 1,
	}, nil
}

// CanView — кто может видеть бронь (дублирует RLS на уровне домена).
func (r Reservation) CanView(p auth.Principal, managesHotel bool) bool {
	switch p.Role {
	case auth.RoleAdmin, auth.RoleSystem:
		return true
	case auth.RoleManager:
		return managesHotel
	default:
		return r.CustomerID == p.UserID
	}
}

// Cancel — правила отмены:
//   - клиент отменяет свою бронь, пока не наступил день заезда;
//   - менеджер своего отеля и админ — пока не наступил день выезда
//     (например, гость не приехал);
//   - system (отключение отеля) — только будущие брони.
func (r *Reservation) Cancel(p auth.Principal, managesHotel bool, reason string, now time.Time, today Date) error {
	if r.Status == Cancelled {
		return apperr.Conflict("already_cancelled", "Бронь уже отменена")
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > CancelReasonL {
		return apperr.Validation(map[string]string{"reason": "Причина не длиннее 500 символов"})
	}
	switch p.Role {
	case auth.RoleCustomer:
		if r.CustomerID != p.UserID {
			return apperr.NotFound("Бронь")
		}
		if !today.Before(r.Stay.CheckIn) {
			return apperr.Conflict("too_late", "Отменить можно только до дня заезда")
		}
	case auth.RoleManager, auth.RoleAdmin:
		if p.Role == auth.RoleManager && !managesHotel {
			return apperr.NotFound("Бронь")
		}
		if !today.Before(r.Stay.CheckOut) {
			return apperr.Conflict("too_late", "Проживание уже завершено")
		}
		if reason == "" {
			return apperr.Validation(map[string]string{"reason": "Укажите причину отмены для гостя"})
		}
	case auth.RoleSystem:
		if !today.Before(r.Stay.CheckIn) {
			return apperr.Conflict("too_late", "Проживание уже началось")
		}
	default:
		return apperr.Forbidden("Нет прав на отмену")
	}
	if reason == "" {
		reason = "Отменено гостем"
	}
	r.Status = Cancelled
	r.CancelledAt = &now
	actor := p.UserID
	if p.Role == auth.RoleSystem {
		r.CancelledBy = nil
	} else {
		r.CancelledBy = &actor
	}
	r.CancelReason = reason
	r.Version++
	return nil
}

// ---------- события ----------

const (
	EventReservationConfirmed = "ReservationConfirmed"
	EventReservationCancelled = "ReservationCancelled"
)

// ReservationEvent — полезная нагрузка обоих событий брони.
type ReservationEvent struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	HotelID       uuid.UUID `json:"hotel_id"`
	HotelName     string    `json:"hotel_name"`
	RoomTypeID    uuid.UUID `json:"room_type_id"`
	RoomTypeName  string    `json:"room_type_name"`
	CustomerID    uuid.UUID `json:"customer_id"`
	CheckIn       string    `json:"check_in"`
	CheckOut      string    `json:"check_out"`
	Guests        int       `json:"guests"`
	Total         Money     `json:"total"`
	Reason        string    `json:"reason,omitempty"`
	CancelledBy   string    `json:"cancelled_by_role,omitempty"`
	// Поля ниже добавлены для проекции booking-query (снимок агрегата
	// целиком, T10). Изменение совместимое: старые потребители их не читают.
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
}

func (r Reservation) Event(cancelledByRole auth.Role) ReservationEvent {
	ev := ReservationEvent{
		ReservationID: r.ID, HotelID: r.HotelID, HotelName: r.HotelName, RoomTypeID: r.RoomTypeID,
		RoomTypeName: r.RoomTypeName, CustomerID: r.CustomerID, CheckIn: r.Stay.CheckIn.String(),
		CheckOut: r.Stay.CheckOut.String(), Guests: r.Guests, Total: r.Total,
		Status: string(r.Status), CreatedAt: r.CreatedAt, CancelledAt: r.CancelledAt,
	}
	if r.Status == Cancelled {
		ev.Reason = r.CancelReason
		ev.CancelledBy = string(cancelledByRole)
	}
	return ev
}
