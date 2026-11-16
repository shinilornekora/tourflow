// Package app — сценарии бронирования.
//
// Ключевое решение: проверка остатков и создание брони — одна локальная
// ACID-транзакция внутри booking. Распределённой транзакции «каталог +
// бронь» нет, потому что booking держит у себя реплику нужных полей
// каталога. Цена решения — реплика может отставать от каталога на время
// доставки события (обычно сотни миллисекунд).
package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
)

// ---------- порты ----------

type DayLoad struct {
	Day    domain.Date
	Booked int
}

type IdemResult struct {
	Fresh        bool // ключ новый — выполняем запрос
	HashMismatch bool // ключ использован с другим телом
	// Response — сохранённый ответ команды: бронь в том виде, в каком её
	// вернул первый запрос. Повтор отдаёт именно его, а не текущее состояние
	// (после отмены оно уже другое) — РХД ПР-4.
	Response domain.Reservation
}

type AuditEntry struct {
	HotelID  uuid.UUID
	Action   string
	EntityID uuid.UUID
	Details  map[string]any
}

type ReservationFilter struct {
	CustomerID uuid.UUID
	HotelID    uuid.UUID
	Status     domain.Status
	// HotelIDs при ScopeHotels=true — отели, которыми ограничен список
	// (отели менеджера); выставляет сервис, не клиент.
	HotelIDs    []uuid.UUID
	ScopeHotels bool
	From, To    domain.Date // пересечение с [From, To)
	Limit       int
	Offset      int
}

// Tx — операции внутри одной транзакции с выставленным контекстом RLS.
type Tx interface {
	ClaimIdempotency(ctx context.Context, customer uuid.UUID, key string, hash []byte) (IdemResult, error)
	// BindIdempotency сохраняет под ключом бронь и ответ команды.
	BindIdempotency(ctx context.Context, customer uuid.UUID, key string, response domain.Reservation) error
	// Offer читает реплику категории и блокирует её строку FOR SHARE, чтобы
	// параллельное событие каталога не изменило total_rooms посреди брони.
	Offer(ctx context.Context, roomType uuid.UUID) (domain.RoomOffer, error)
	// LockDays создаёт недостающие строки остатков и блокирует их по
	// возрастанию даты.
	LockDays(ctx context.Context, roomType uuid.UUID, stay domain.Stay) ([]DayLoad, error)
	AddBooked(ctx context.Context, roomType uuid.UUID, stay domain.Stay, delta int) error
	InsertReservation(ctx context.Context, r domain.Reservation) error
	UpdateReservation(ctx context.Context, r domain.Reservation, expectedVersion int64) error
	Reservation(ctx context.Context, id uuid.UUID, forUpdate bool) (domain.Reservation, error)
	ManagesHotel(ctx context.Context, hotel, user uuid.UUID) (bool, error)
	Audit(ctx context.Context, actor auth.Principal, e AuditEntry) error
	// Outbox кладёт событие в очередь той же транзакцией; hotel — арендатор
	// события (для разбора и выборок по отелю).
	Outbox(ctx context.Context, hotel uuid.UUID, env messaging.Envelope) error
}

type Store interface {
	InTx(ctx context.Context, actor auth.Principal, fn func(Tx) error) error
	ListReservations(ctx context.Context, actor auth.Principal, f ReservationFilter) ([]domain.Reservation, int, error)
	Availability(ctx context.Context, hotel uuid.UUID, stay domain.Stay, guests int) ([]Availability, error)
	AuditLog(ctx context.Context, actor auth.Principal, hotel uuid.UUID, limit, offset int) ([]AuditRecord, int, error)
	ManagedHotelIDs(ctx context.Context, user uuid.UUID) ([]uuid.UUID, error)
}

type Availability struct {
	RoomTypeID uuid.UUID    `json:"room_type_id"`
	Name       string       `json:"name"`
	Capacity   int          `json:"capacity"`
	TotalRooms int          `json:"total_rooms"`
	Free       int          `json:"free"`
	PerNight   domain.Money `json:"price_per_night"`
	Total      domain.Money `json:"total_price"`
}

type AuditRecord struct {
	ID        int64          `json:"id"`
	HotelID   uuid.UUID      `json:"hotel_id"`
	ActorID   *uuid.UUID     `json:"actor_id"`
	ActorRole string         `json:"actor_role"`
	Action    string         `json:"action"`
	EntityID  uuid.UUID      `json:"entity_id"`
	Details   map[string]any `json:"details"`
	At        time.Time      `json:"at"`
}

// ---------- сервис ----------

type Service struct {
	Store    Store
	Location *time.Location
	Now      func() time.Time
}

const producer = "booking"

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) today() domain.Date {
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	return domain.Today(s.now(), loc)
}

type BookInput struct {
	RoomTypeID     uuid.UUID
	CheckIn        domain.Date
	CheckOut       domain.Date
	Guests         int
	IdempotencyKey string
}

func (in BookInput) hash() []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", in.RoomTypeID, in.CheckIn, in.CheckOut, in.Guests)))
	return sum[:]
}

// Book создаёт бронь. replayed=true — это повтор уже выполненного запроса
// с тем же Idempotency-Key: вернули ранее созданную бронь, ничего не меняли.
func (s *Service) Book(ctx context.Context, in BookInput) (res domain.Reservation, replayed bool, err error) {
	p, err := auth.RequireRole(ctx, auth.RoleCustomer)
	if err != nil {
		return res, false, err
	}
	if l := len(in.IdempotencyKey); l < 8 || l > 128 {
		return res, false, apperr.New(apperr.KindValidation, "idempotency_key_required",
			"Нужен заголовок Idempotency-Key (8–128 символов)")
	}
	err = s.Store.InTx(ctx, p, func(tx Tx) error {
		// Ключ проверяется раньше дат: повтор уже выполненного запроса обязан
		// вернуть сохранённый ответ, даже если за это время наступил новый
		// день и дата заезда «сегодня» стала вчерашней.
		claim, err := tx.ClaimIdempotency(ctx, p.UserID, in.IdempotencyKey, in.hash())
		if err != nil {
			return err
		}
		switch {
		case claim.HashMismatch:
			return apperr.New(apperr.KindValidation, "idempotency_key_reused",
				"Этот Idempotency-Key уже использован для другого запроса")
		case !claim.Fresh:
			res, replayed = claim.Response, true
			return nil
		}
		// Ошибка проверки дат откатывает транзакцию вместе с захваченным ключом.
		stay, err := domain.NewStay(in.CheckIn, in.CheckOut, s.today())
		if err != nil {
			return err
		}

		offer, err := tx.Offer(ctx, in.RoomTypeID)
		if err != nil {
			return err
		}
		res, err = domain.NewReservation(p, offer, stay, in.Guests, s.now())
		if err != nil {
			return err
		}
		days, err := tx.LockDays(ctx, offer.RoomTypeID, stay)
		if err != nil {
			return err
		}
		for _, d := range days {
			if d.Booked >= offer.TotalRooms {
				return apperr.Conflict("no_availability",
					fmt.Sprintf("На %s свободных номеров этой категории не осталось", d.Day))
			}
		}
		if err := tx.AddBooked(ctx, offer.RoomTypeID, stay, +1); err != nil {
			return err
		}
		if err := tx.InsertReservation(ctx, res); err != nil {
			return err
		}
		if err := tx.BindIdempotency(ctx, p.UserID, in.IdempotencyKey, res); err != nil {
			return err
		}
		if err := tx.Audit(ctx, p, AuditEntry{HotelID: res.HotelID, Action: "reservation.created", EntityID: res.ID,
			Details: map[string]any{"check_in": stay.CheckIn.String(), "check_out": stay.CheckOut.String(), "guests": res.Guests, "total": res.Total.Amount}}); err != nil {
			return err
		}
		return s.emit(ctx, tx, domain.EventReservationConfirmed, res, p.Role)
	})
	return res, replayed, err
}

func (s *Service) emit(ctx context.Context, tx Tx, eventType string, r domain.Reservation, role auth.Role) error {
	env, err := messaging.NewEnvelope(ctx, producer, eventType, r.ID, r.Version, r.Event(role))
	if err != nil {
		return err
	}
	return tx.Outbox(ctx, r.HotelID, env)
}

// Cancel отменяет бронь и возвращает номер в продажу.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, reason string) (domain.Reservation, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return domain.Reservation{}, err
	}
	var res domain.Reservation
	err = s.Store.InTx(ctx, p, func(tx Tx) error {
		res, err = s.cancelInTx(ctx, tx, p, id, reason)
		return err
	})
	return res, err
}

func (s *Service) cancelInTx(ctx context.Context, tx Tx, p auth.Principal, id uuid.UUID, reason string) (domain.Reservation, error) {
	// Порядок блокировок тот же, что у Book: сначала строки остатков по
	// возрастанию даты, потом бронь. Иначе встречные Book и Cancel по одной
	// категории могут взаимно заблокироваться.
	res, err := tx.Reservation(ctx, id, false)
	if err != nil {
		return res, err
	}
	if _, err := tx.LockDays(ctx, res.RoomTypeID, res.Stay); err != nil {
		return res, err
	}
	res, err = tx.Reservation(ctx, id, true)
	if err != nil {
		return res, err
	}
	manages := false
	if p.Role == auth.RoleManager {
		if manages, err = tx.ManagesHotel(ctx, res.HotelID, p.UserID); err != nil {
			return res, err
		}
	}
	expected := res.Version
	if err := res.Cancel(p, manages, reason, s.now(), s.today()); err != nil {
		return res, err
	}
	if err := tx.AddBooked(ctx, res.RoomTypeID, res.Stay, -1); err != nil {
		return res, err
	}
	if err := tx.UpdateReservation(ctx, res, expected); err != nil {
		return res, err
	}
	if err := tx.Audit(ctx, p, AuditEntry{HotelID: res.HotelID, Action: "reservation.cancelled", EntityID: res.ID,
		Details: map[string]any{"reason": res.CancelReason}}); err != nil {
		return res, err
	}
	return res, s.emit(ctx, tx, domain.EventReservationCancelled, res, p.Role)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (domain.Reservation, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return domain.Reservation{}, err
	}
	var res domain.Reservation
	err = s.Store.InTx(ctx, p, func(tx Tx) error {
		res, err = tx.Reservation(ctx, id, false)
		if err != nil {
			return err
		}
		manages := false
		if p.Role == auth.RoleManager {
			manages, err = tx.ManagesHotel(ctx, res.HotelID, p.UserID)
			if err != nil {
				return err
			}
		}
		if !res.CanView(p, manages) {
			return apperr.NotFound("Бронь")
		}
		return nil
	})
	return res, err
}

// My — «корзина» клиента: текущие и прошлые брони.
func (s *Service) My(ctx context.Context, f ReservationFilter) ([]domain.Reservation, int, error) {
	p, err := auth.RequireRole(ctx, auth.RoleCustomer)
	if err != nil {
		return nil, 0, err
	}
	f.CustomerID = p.UserID
	return s.Store.ListReservations(ctx, p, f)
}

// HotelReservations — брони отеля для менеджера и администратора.
func (s *Service) HotelReservations(ctx context.Context, f ReservationFilter) ([]domain.Reservation, int, error) {
	p, err := auth.RequireRole(ctx, auth.RoleManager, auth.RoleAdmin)
	if err != nil {
		return nil, 0, err
	}
	f.CustomerID = uuid.Nil
	// Уровень приложения: менеджеру — только его отели. RLS проверит то же
	// самое независимо (уровень базы), но полагаться на неё одну нельзя.
	if p.Role == auth.RoleManager {
		own, ok, err := s.managerScope(ctx, p, f.HotelID)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			return []domain.Reservation{}, 0, nil
		}
		f.HotelIDs, f.ScopeHotels = own, true
	}
	return s.Store.ListReservations(ctx, p, f)
}

// managerScope возвращает отели менеджера. ok=false — запрошен чужой отель
// или отелей нет: ответ пустой, как и для несуществующего отеля (не выдаём,
// что отель есть).
func (s *Service) managerScope(ctx context.Context, p auth.Principal, hotel uuid.UUID) ([]uuid.UUID, bool, error) {
	own, err := s.Store.ManagedHotelIDs(ctx, p.UserID)
	if err != nil {
		return nil, false, err
	}
	if hotel == uuid.Nil {
		return own, len(own) > 0, nil
	}
	for _, id := range own {
		if id == hotel {
			return own, true, nil
		}
	}
	return nil, false, nil
}

// Availability — что можно забронировать на даты. Клиенту и анониму
// возвращаются только категории со свободными номерами: занятые на эти даты
// клиент не видит вообще.
func (s *Service) Availability(ctx context.Context, hotel uuid.UUID, in, out domain.Date, guests int) ([]Availability, error) {
	stay, err := domain.NewStay(in, out, s.today())
	if err != nil {
		return nil, err
	}
	if guests < 1 {
		guests = 1
	}
	list, err := s.Store.Availability(ctx, hotel, stay, guests)
	if err != nil {
		return nil, err
	}
	p, authed := auth.FromContext(ctx)
	if authed && p.Is(auth.RoleAdmin, auth.RoleManager) {
		return list, nil
	}
	out2 := list[:0]
	for _, a := range list {
		if a.Free > 0 {
			out2 = append(out2, a)
		}
	}
	return out2, nil
}

func (s *Service) AuditLog(ctx context.Context, hotel uuid.UUID, limit, offset int) ([]AuditRecord, int, error) {
	p, err := auth.RequireRole(ctx, auth.RoleManager, auth.RoleAdmin)
	if err != nil {
		return nil, 0, err
	}
	if p.Role == auth.RoleManager {
		if _, ok, err := s.managerScope(ctx, p, hotel); err != nil {
			return nil, 0, err
		} else if !ok {
			return []AuditRecord{}, 0, nil
		}
	}
	return s.Store.AuditLog(ctx, p, hotel, limit, offset)
}

func (s *Service) ManagedHotelIDs(ctx context.Context) ([]uuid.UUID, error) {
	p, err := auth.RequireRole(ctx, auth.RoleManager)
	if err != nil {
		return nil, err
	}
	return s.Store.ManagedHotelIDs(ctx, p.UserID)
}
