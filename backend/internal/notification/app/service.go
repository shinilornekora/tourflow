// Package app — уведомления гостей о событиях брони.
//
// Сервис ничего не знает о бронях, кроме событий: он не может сломать
// бронирование и не замедляет его. Если notification лежит, брони
// продолжают создаваться, а уведомления догонят после подъёма — события
// ждут в Kafka.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
)

type Notification struct {
	ID            uuid.UUID  `json:"id"`
	UserID        uuid.UUID  `json:"-"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	ReservationID *uuid.UUID `json:"reservation_id,omitempty"`
	SourceEvent   uuid.UUID  `json:"-"`
	CreatedAt     time.Time  `json:"created_at"`
	ReadAt        *time.Time `json:"read_at,omitempty"`
}

type Store interface {
	// Deliver атомарно отмечает событие обработанным и сохраняет уведомление.
	Deliver(ctx context.Context, env messaging.Envelope, n Notification) (bool, error)
	List(ctx context.Context, user uuid.UUID, unreadOnly bool, limit, offset int) ([]Notification, int, error)
	Unread(ctx context.Context, user uuid.UUID) (int, error)
	MarkRead(ctx context.Context, user, id uuid.UUID) error
	MarkAllRead(ctx context.Context, user uuid.UUID) (int64, error)
}

// Channel — внешний канал доставки (email/SMS). В учебной сборке — журнал.
type Channel interface {
	Send(ctx context.Context, n Notification) error
}

type Service struct {
	Store   Store
	Channel Channel
	Log     *slog.Logger
	Now     func() time.Time
}

// reservationEvent — то, что notification читает из события брони.
type reservationEvent struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	HotelName     string    `json:"hotel_name"`
	RoomTypeName  string    `json:"room_type_name"`
	CustomerID    uuid.UUID `json:"customer_id"`
	CheckIn       string    `json:"check_in"`
	CheckOut      string    `json:"check_out"`
	Total         struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	} `json:"total"`
	Reason      string `json:"reason"`
	CancelledBy string `json:"cancelled_by_role"`
}

func rub(amount int64) string { return fmt.Sprintf("%d,%02d ₽", amount/100, amount%100) }

func (s *Service) HandleReservationEvent(ctx context.Context, env messaging.Envelope) error {
	ev, err := messaging.Decode[reservationEvent](env)
	if err != nil {
		return err
	}
	if ev.CustomerID == uuid.Nil {
		return fmt.Errorf("%w: event without customer_id", messaging.ErrPermanent)
	}
	n := Notification{ID: uuid.New(), UserID: ev.CustomerID, SourceEvent: env.EventID, CreatedAt: time.Now().UTC()}
	rid := ev.ReservationID
	n.ReservationID = &rid
	switch env.EventType {
	case "ReservationConfirmed":
		n.Kind = "reservation_confirmed"
		n.Title = "Бронь подтверждена"
		n.Body = fmt.Sprintf("%s, %s: с %s по %s. Сумма %s.", ev.HotelName, ev.RoomTypeName, ev.CheckIn, ev.CheckOut, rub(ev.Total.Amount))
	case "ReservationCancelled":
		n.Kind = "reservation_cancelled"
		n.Title = "Бронь отменена"
		who := map[string]string{"customer": "вами", "hotel_manager": "отелем", "admin": "администратором", "system": "системой"}[ev.CancelledBy]
		if who == "" {
			who = "системой"
		}
		n.Body = fmt.Sprintf("%s, %s (%s — %s) отменена %s. Причина: %s.", ev.HotelName, ev.RoomTypeName, ev.CheckIn, ev.CheckOut, who, ev.Reason)
	default:
		return nil
	}
	fresh, err := s.Store.Deliver(ctx, env, n)
	if err != nil || !fresh {
		return err
	}
	// Внешний канал — после фиксации в БД. Если отправка упадёт, уведомление
	// в личном кабинете уже есть; повторно письмо не уйдёт (at-most-once для
	// email). Для учебной системы это честный компромисс; в бою здесь был бы
	// свой outbox канала доставки.
	if s.Channel != nil {
		if err := s.Channel.Send(ctx, n); err != nil {
			s.Log.Warn("external channel failed", "notification_id", n.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) List(ctx context.Context, unreadOnly bool, limit, offset int) ([]Notification, int, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.Store.List(ctx, p.UserID, unreadOnly, limit, offset)
}

func (s *Service) Unread(ctx context.Context) (int, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return 0, err
	}
	return s.Store.Unread(ctx, p.UserID)
}

func (s *Service) MarkRead(ctx context.Context, id uuid.UUID) error {
	p, err := auth.Require(ctx)
	if err != nil {
		return err
	}
	return s.Store.MarkRead(ctx, p.UserID, id)
}

func (s *Service) MarkAllRead(ctx context.Context) (int64, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return 0, err
	}
	return s.Store.MarkAllRead(ctx, p.UserID)
}

// LogChannel «отправляет» письма в журнал.
type LogChannel struct{ Log *slog.Logger }

func (c LogChannel) Send(ctx context.Context, n Notification) error {
	c.Log.InfoContext(ctx, "email sent (simulated)", "user_id", n.UserID, "title", n.Title)
	return nil
}
