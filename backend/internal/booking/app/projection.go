package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
)

// Контракты событий каталога глазами booking. Структуры объявлены здесь,
// а не импортируются из catalog: сервисы не делят код доменной модели.
// Читаем только нужные поля, лишние игнорируем (tolerant reader) — каталог
// может добавлять поля в событие, не ломая booking.

type HotelSnapshot struct {
	HotelID    uuid.UUID   `json:"hotel_id"`
	Name       string      `json:"name"`
	City       string      `json:"city"`
	Status     string      `json:"status"`
	ManagerIDs []uuid.UUID `json:"manager_ids"`
	Version    int64       `json:"version"`
	// ChangedBy — автор изменения в каталоге; поле необязательное.
	ChangedBy *uuid.UUID `json:"changed_by,omitempty"`
}

type RoomTypeSnapshot struct {
	RoomTypeID uuid.UUID    `json:"room_type_id"`
	HotelID    uuid.UUID    `json:"hotel_id"`
	Name       string       `json:"name"`
	Capacity   int          `json:"capacity"`
	TotalRooms int          `json:"total_rooms"`
	Price      domain.Money `json:"price"`
	Status     string       `json:"status"`
	Version    int64        `json:"version"`
}

// ProjectionTx — операции проекции внутри транзакции обработки события.
type ProjectionTx interface {
	Tx
	MarkProcessed(ctx context.Context, env messaging.Envelope) (bool, error)
	// ApplyHotel применяет снимок, только если он новее сохранённого.
	ApplyHotel(ctx context.Context, s HotelSnapshot) (applied bool, prevStatus string, err error)
	ApplyRoomType(ctx context.Context, s RoomTypeSnapshot) (applied bool, err error)
	FutureConfirmed(ctx context.Context, hotel uuid.UUID, after domain.Date) ([]uuid.UUID, error)
}

type ProjectionStore interface {
	InProjectionTx(ctx context.Context, fn func(ProjectionTx) error) error
}

var system = auth.Principal{Role: auth.RoleSystem}

// HandleCatalogEvent — обработчик событий каталога.
//
// Идемпотентность двойная: event_id в processed_events отсекает точный
// повтор, сравнение version отсекает устаревший снимок, пришедший позже
// нового (например, после ребалансировки группы потребителей).
func (s *Service) HandleCatalogEvent(ctx context.Context, ps ProjectionStore, env messaging.Envelope) error {
	return ps.InProjectionTx(ctx, func(tx ProjectionTx) error {
		fresh, err := tx.MarkProcessed(ctx, env)
		if err != nil || !fresh {
			return err
		}
		switch env.EventType {
		case "HotelUpserted":
			snap, err := messaging.Decode[HotelSnapshot](env)
			if err != nil {
				return err
			}
			applied, prev, err := tx.ApplyHotel(ctx, snap)
			if err != nil || !applied {
				return err
			}
			if prev == "active" && snap.Status == "inactive" {
				return s.cancelFutureForHotel(ctx, tx, snap.HotelID, snap.ChangedBy)
			}
			return nil
		case "RoomTypeUpserted":
			snap, err := messaging.Decode[RoomTypeSnapshot](env)
			if err != nil {
				return err
			}
			// Архивирование категории не отменяет существующие брони:
			// клиент купил номер, отель обязан его предоставить. Категория
			// только перестаёт продаваться.
			_, err = tx.ApplyRoomType(ctx, snap)
			return err
		default:
			return nil // неизвестные типы событий пропускаем
		}
	})
}

// cancelFutureForHotel — компенсация при отключении отеля: все брони, заезд
// по которым ещё не наступил, отменяются от имени system. Каждая отмена
// порождает ReservationCancelled → notification сообщит гостям.
// Брони с заездом сегодня и раньше не трогаем: день заезда считается уже
// начавшимся проживанием (гость может быть в пути или в номере) — такие
// случаи отель решает вручную.
func (s *Service) cancelFutureForHotel(ctx context.Context, tx ProjectionTx, hotel uuid.UUID, by *uuid.UUID) error {
	ids, err := tx.FutureConfirmed(ctx, hotel, s.today())
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.cancelInTx(ctx, tx, system, id, "Отель временно не принимает гостей"); err != nil {
			return err
		}
	}
	// Хореография не даёт каталогу ответа «компенсация выполнена» (T23, T121).
	// Единственный след — эта запись аудита отеля: администратор видит, что
	// отключение дошло до booking и сколько броней оно отменило. Если записи
	// нет, событие не обработано (ищите его в DLQ или в отставании группы).
	// Запись идёт от имени system (hotel.* другим ролям запрещены политикой),
	// а кто отключил отель в каталоге — в деталях: у каталога своего журнала нет.
	details := map[string]any{"cancelled_reservations": len(ids)}
	if by != nil {
		details["deactivated_by"] = by.String()
	}
	return tx.Audit(ctx, system, AuditEntry{HotelID: hotel, Action: "hotel.deactivation_processed", EntityID: hotel, Details: details})
}
