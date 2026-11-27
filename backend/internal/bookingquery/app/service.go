// Package app — модель чтения броней (booking-query).
//
// Сервис не принимает команд: всё, что он знает, пришло событиями.
// booking остаётся владельцем факта (бронь, остатки, аудит), а здесь
// хранится проекция, удобная для экранов: карточки, списки, сводки.
//
// Цена решения — согласованность «в конечном счёте»: после POST в booking
// карточка появляется здесь через время доставки события (обычно сотни
// миллисекунд). Клиент, которому нужен результат команды сразу, берёт его
// из ответа самой команды.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
)

// ---------- модель чтения ----------

type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// Card — карточка брони. Поля JSON совпадают с ответом booking, чтобы SPA
// не различала, откуда пришла бронь.
type Card struct {
	ID              uuid.UUID  `json:"id"`
	HotelID         uuid.UUID  `json:"hotel_id"`
	HotelName       string     `json:"hotel_name"`
	RoomTypeID      uuid.UUID  `json:"room_type_id"`
	RoomTypeName    string     `json:"room_type_name"`
	CustomerID      uuid.UUID  `json:"customer_id"`
	CheckIn         string     `json:"check_in"`
	CheckOut        string     `json:"check_out"`
	Nights          int        `json:"nights"`
	Guests          int        `json:"guests"`
	Status          string     `json:"status"`
	Total           Money      `json:"total"`
	CreatedAt       time.Time  `json:"created_at"`
	CancelledAt     *time.Time `json:"cancelled_at,omitempty"`
	CancelReason    string     `json:"cancel_reason,omitempty"`
	CancelledByRole string     `json:"cancelled_by_role,omitempty"`
	Version         int64      `json:"version"`
}

const (
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// Filter — условия выборки списка. Даты — пересечение проживания с [From, To).
type Filter struct {
	CustomerID uuid.UUID
	HotelID    uuid.UUID
	// HotelIDs при ScopeHotels=true — отели менеджера; выставляет сервис.
	HotelIDs    []uuid.UUID
	ScopeHotels bool
	Status      string
	From, To    time.Time
	Limit       int
	Offset      int
}

// HotelStats — сводка по одному отелю.
type HotelStats struct {
	HotelID   uuid.UUID `json:"hotel_id"`
	HotelName string    `json:"hotel_name"`
	Confirmed int       `json:"confirmed"`
	Cancelled int       `json:"cancelled"`
	Nights    int       `json:"nights"`
	Revenue   Money     `json:"revenue"`
}

// HotelSummary — сводка для менеджера и администратора.
type HotelSummary struct {
	Confirmed int          `json:"confirmed"`
	Cancelled int          `json:"cancelled"`
	Upcoming  int          `json:"upcoming"`
	Nights    int          `json:"nights"`
	Guests    int          `json:"guests"`
	Revenue   Money        `json:"revenue"`
	ByHotel   []HotelStats `json:"by_hotel"`
}

// CustomerSummary — сводка личного кабинета клиента.
type CustomerSummary struct {
	Upcoming   int   `json:"upcoming"`
	InProgress int   `json:"in_progress"`
	Completed  int   `json:"completed"`
	Cancelled  int   `json:"cancelled"`
	TotalSpent Money `json:"total_spent"`
	// NextCheckIn — ближайший заезд по подтверждённой брони.
	NextCheckIn *string `json:"next_check_in"`
}

// ---------- порты ----------

// HotelSnapshot — то, что проекция читает из события каталога. Структура
// своя, не из catalog (tolerant reader, T11).
type HotelSnapshot struct {
	HotelID    uuid.UUID   `json:"hotel_id"`
	Name       string      `json:"name"`
	City       string      `json:"city"`
	Status     string      `json:"status"`
	ManagerIDs []uuid.UUID `json:"manager_ids"`
	Version    int64       `json:"version"`
}

// ProjectionTx — операции проектора в транзакции обработки события.
type ProjectionTx interface {
	// MarkProcessed — inbox; false: событие уже обработано.
	MarkProcessed(ctx context.Context, env messaging.Envelope) (bool, error)
	// UpsertCard применяет карточку, только если её версия новее.
	UpsertCard(ctx context.Context, c Card) (bool, error)
	// ApplyHotel применяет снимок отеля и менеджеров, только если он новее.
	ApplyHotel(ctx context.Context, s HotelSnapshot) (bool, error)
}

type Store interface {
	InProjectionTx(ctx context.Context, fn func(ProjectionTx) error) error
	List(ctx context.Context, actor auth.Principal, f Filter) ([]Card, int, error)
	Get(ctx context.Context, actor auth.Principal, id uuid.UUID) (Card, error)
	ManagedHotelIDs(ctx context.Context, actor auth.Principal) ([]uuid.UUID, error)
	HotelSummary(ctx context.Context, actor auth.Principal, f Filter, today time.Time) (HotelSummary, error)
	CustomerSummary(ctx context.Context, actor auth.Principal, customer uuid.UUID, today time.Time) (CustomerSummary, error)
}

// ---------- сервис ----------

type Service struct {
	Store    Store
	Location *time.Location
	Now      func() time.Time
}

func (s *Service) today() time.Time {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ---------- проектор ----------

// reservationEvent — событие брони глазами проекции.
type reservationEvent struct {
	ReservationID uuid.UUID  `json:"reservation_id"`
	HotelID       uuid.UUID  `json:"hotel_id"`
	HotelName     string     `json:"hotel_name"`
	RoomTypeID    uuid.UUID  `json:"room_type_id"`
	RoomTypeName  string     `json:"room_type_name"`
	CustomerID    uuid.UUID  `json:"customer_id"`
	CheckIn       string     `json:"check_in"`
	CheckOut      string     `json:"check_out"`
	Guests        int        `json:"guests"`
	Total         Money      `json:"total"`
	Reason        string     `json:"reason"`
	CancelledBy   string     `json:"cancelled_by_role"`
	Status        string     `json:"status"`
	CreatedAt     *time.Time `json:"created_at"`
	CancelledAt   *time.Time `json:"cancelled_at"`
}

// CardFromEvent строит карточку из конверта события брони. Ошибка —
// постоянная (ErrPermanent): событие нарушает контракт и уйдёт в DLQ.
//
// Поля status, created_at, cancelled_at booking добавил в событие позже;
// для старых событий они выводятся из типа события и occurred_at.
func CardFromEvent(env messaging.Envelope) (Card, error) {
	ev, err := messaging.Decode[reservationEvent](env)
	if err != nil {
		return Card{}, err
	}
	bad := func(why string) (Card, error) {
		return Card{}, fmt.Errorf("%w: %s event: %s", messaging.ErrPermanent, env.EventType, why)
	}
	if ev.ReservationID == uuid.Nil || ev.HotelID == uuid.Nil || ev.CustomerID == uuid.Nil || ev.RoomTypeID == uuid.Nil {
		return bad("reservation_id, hotel_id, room_type_id and customer_id are required")
	}
	if ev.ReservationID != env.AggregateID {
		return bad("reservation_id differs from aggregate_id")
	}
	in, err1 := time.Parse(time.DateOnly, ev.CheckIn)
	out, err2 := time.Parse(time.DateOnly, ev.CheckOut)
	if err1 != nil || err2 != nil || !out.After(in) {
		return bad("check_in/check_out must be dates with check_out > check_in")
	}
	status := ev.Status
	if status == "" {
		status = map[string]string{"ReservationConfirmed": StatusConfirmed, "ReservationCancelled": StatusCancelled}[env.EventType]
	}
	if status != StatusConfirmed && status != StatusCancelled {
		return bad("unknown status " + status)
	}
	if ev.Total.Currency == "" {
		ev.Total.Currency = "RUB"
	}
	c := Card{
		ID: ev.ReservationID, HotelID: ev.HotelID, HotelName: ev.HotelName, RoomTypeID: ev.RoomTypeID,
		RoomTypeName: ev.RoomTypeName, CustomerID: ev.CustomerID, CheckIn: ev.CheckIn, CheckOut: ev.CheckOut,
		Nights: int(out.Sub(in).Hours() / 24), Guests: ev.Guests, Status: status, Total: ev.Total,
		Version: env.AggregateVersion,
	}
	if ev.CreatedAt != nil {
		c.CreatedAt = ev.CreatedAt.UTC()
	} else if status == StatusConfirmed {
		c.CreatedAt = env.OccurredAt.UTC()
	} // иначе момент создания неизвестен — store сохранит прежний
	if status == StatusCancelled {
		at := env.OccurredAt.UTC()
		if ev.CancelledAt != nil {
			at = ev.CancelledAt.UTC()
		}
		c.CancelledAt = &at
		c.CancelReason = ev.Reason
		c.CancelledByRole = ev.CancelledBy
	}
	return c, nil
}

// HandleReservationEvent — проекция booking.reservation.v1.
//
// Алгоритм тот же, что у остальных потребителей (ARCHITECTURE, «Контракт
// событий»): inbox → сравнение версии → эффект, одной транзакцией.
func (s *Service) HandleReservationEvent(ctx context.Context, env messaging.Envelope) error {
	switch env.EventType {
	case "ReservationConfirmed", "ReservationCancelled":
	default:
		return nil // неизвестные типы пропускаем
	}
	card, err := CardFromEvent(env)
	if err != nil {
		return err
	}
	return s.Store.InProjectionTx(ctx, func(tx ProjectionTx) error {
		fresh, err := tx.MarkProcessed(ctx, env)
		if err != nil || !fresh {
			return err
		}
		_, err = tx.UpsertCard(ctx, card)
		return err
	})
}

// HandleHotelEvent — назначения менеджеров из catalog.hotel.v1. По ним
// решается, какие брони видит менеджер.
func (s *Service) HandleHotelEvent(ctx context.Context, env messaging.Envelope) error {
	if env.EventType != "HotelUpserted" {
		return nil
	}
	snap, err := messaging.Decode[HotelSnapshot](env)
	if err != nil {
		return err
	}
	if snap.HotelID == uuid.Nil {
		return fmt.Errorf("%w: HotelUpserted without hotel_id", messaging.ErrPermanent)
	}
	if snap.Version == 0 {
		snap.Version = env.AggregateVersion
	}
	return s.Store.InProjectionTx(ctx, func(tx ProjectionTx) error {
		fresh, err := tx.MarkProcessed(ctx, env)
		if err != nil || !fresh {
			return err
		}
		_, err = tx.ApplyHotel(ctx, snap)
		return err
	})
}

// Handle — единый обработчик для потребителя обоих топиков.
func (s *Service) Handle(ctx context.Context, env messaging.Envelope) error {
	switch env.EventType {
	case "HotelUpserted":
		return s.HandleHotelEvent(ctx, env)
	default:
		return s.HandleReservationEvent(ctx, env)
	}
}

// ---------- запросы ----------

// My — брони клиента (личный кабинет: текущие и история).
func (s *Service) My(ctx context.Context, f Filter) ([]Card, int, error) {
	p, err := auth.RequireRole(ctx, auth.RoleCustomer)
	if err != nil {
		return nil, 0, err
	}
	f.CustomerID, f.HotelIDs, f.ScopeHotels = p.UserID, nil, false
	return s.Store.List(ctx, p, f)
}

// MySummary — сводка личного кабинета.
func (s *Service) MySummary(ctx context.Context) (CustomerSummary, error) {
	p, err := auth.RequireRole(ctx, auth.RoleCustomer)
	if err != nil {
		return CustomerSummary{}, err
	}
	return s.Store.CustomerSummary(ctx, p, p.UserID, s.today())
}

// HotelReservations — брони отелей для менеджера (только свои) и админа.
func (s *Service) HotelReservations(ctx context.Context, f Filter) ([]Card, int, error) {
	p, err := auth.RequireRole(ctx, auth.RoleManager, auth.RoleAdmin)
	if err != nil {
		return nil, 0, err
	}
	f.CustomerID = uuid.Nil
	ok, err := s.scope(ctx, p, &f)
	if err != nil || !ok {
		return []Card{}, 0, err
	}
	return s.Store.List(ctx, p, f)
}

// HotelSummary — сводка по броням отелей за период.
func (s *Service) HotelSummary(ctx context.Context, f Filter) (HotelSummary, error) {
	p, err := auth.RequireRole(ctx, auth.RoleManager, auth.RoleAdmin)
	if err != nil {
		return HotelSummary{}, err
	}
	f.CustomerID = uuid.Nil
	ok, err := s.scope(ctx, p, &f)
	if err != nil {
		return HotelSummary{}, err
	}
	if !ok {
		return HotelSummary{Revenue: Money{Currency: "RUB"}, ByHotel: []HotelStats{}}, nil
	}
	return s.Store.HotelSummary(ctx, p, f, s.today())
}

// scope ограничивает выборку менеджера его отелями (уровень приложения;
// RLS проверит то же независимо). ok=false — чужой отель или отелей нет:
// ответ пустой, как для несуществующего отеля.
func (s *Service) scope(ctx context.Context, p auth.Principal, f *Filter) (bool, error) {
	if p.Role != auth.RoleManager {
		return true, nil
	}
	own, err := s.Store.ManagedHotelIDs(ctx, p)
	if err != nil {
		return false, err
	}
	if f.HotelID != uuid.Nil {
		found := false
		for _, id := range own {
			found = found || id == f.HotelID
		}
		if !found {
			return false, nil
		}
	}
	f.HotelIDs, f.ScopeHotels = own, true
	return len(own) > 0, nil
}

// Get — карточка брони. Чужая бронь неотличима от несуществующей (404).
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Card, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return Card{}, err
	}
	c, err := s.Store.Get(ctx, p, id)
	if err != nil {
		return c, err
	}
	// Второй уровень после RLS: проверка в коде не зависит от политики БД.
	if p.Role == auth.RoleCustomer && c.CustomerID != p.UserID {
		return Card{}, apperr.NotFound("Бронь")
	}
	if p.Role == auth.RoleManager {
		own, err := s.Store.ManagedHotelIDs(ctx, p)
		if err != nil {
			return Card{}, err
		}
		for _, h := range own {
			if h == c.HotelID {
				return c, nil
			}
		}
		return Card{}, apperr.NotFound("Бронь")
	}
	return c, nil
}
