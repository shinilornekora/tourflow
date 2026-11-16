// Package app — сценарии каталога.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/catalog/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
)

type HotelFilter struct {
	Query           string
	City            string
	StarsMin        int
	PriceMax        int64
	Guests          int
	Sort            string // name | -stars | price | -created_at
	IncludeInactive bool
	ManagedBy       uuid.UUID
	Limit, Offset   int
}

// HotelSummary — строка списка: отель + минимальная цена активных категорий.
type HotelSummary struct {
	domain.Hotel
	MinPrice      *domain.Money
	RoomTypeCount int
}

// Store — порт хранилища. Методы Save* меняют запись только если версия в БД
// равна expectedVersion, и в той же транзакции кладут событие в outbox.
type Store interface {
	CreateHotel(ctx context.Context, h domain.Hotel, ev messaging.Envelope) error
	SaveHotel(ctx context.Context, h domain.Hotel, expectedVersion int64, ev messaging.Envelope) error
	Hotel(ctx context.Context, id uuid.UUID) (domain.Hotel, error)
	ListHotels(ctx context.Context, f HotelFilter) ([]HotelSummary, int, error)
	Cities(ctx context.Context) ([]string, error)

	CreateRoomType(ctx context.Context, r domain.RoomType, ev messaging.Envelope) error
	SaveRoomType(ctx context.Context, r domain.RoomType, expectedVersion int64, ev messaging.Envelope) error
	RoomType(ctx context.Context, id uuid.UUID) (domain.RoomType, error)
	RoomTypes(ctx context.Context, hotelID uuid.UUID, includeArchived bool) ([]domain.RoomType, error)
}

// Directory — порт к identity: узнать роль пользователя.
type Directory interface {
	Role(ctx context.Context, userID uuid.UUID) (auth.Role, error)
}

type Service struct {
	Store     Store
	Directory Directory
	Now       func() time.Time
}

const producer = "catalog"

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func hotelEvent(ctx context.Context, h domain.Hotel) (messaging.Envelope, error) {
	snap := h.Snapshot()
	if p, ok := auth.FromContext(ctx); ok && p.UserID != uuid.Nil {
		id := p.UserID
		snap.ChangedBy = &id
	}
	return messaging.NewEnvelope(ctx, producer, domain.EventHotelUpserted, h.ID, h.Version, snap)
}

func roomEvent(ctx context.Context, r domain.RoomType) (messaging.Envelope, error) {
	return messaging.NewEnvelope(ctx, producer, domain.EventRoomTypeUpserted, r.ID, r.Version, r.Snapshot())
}

func versionCheck(h *domain.Hotel, clientVersion int64) error {
	if clientVersion != 0 && clientVersion != h.Version {
		return apperr.Conflict("version_mismatch", "Отель изменился в другом окне, обновите страницу")
	}
	return nil
}

// ---------- отели ----------

func (s *Service) CreateHotel(ctx context.Context, f domain.HotelFields) (domain.Hotel, error) {
	if _, err := auth.RequireRole(ctx, auth.RoleAdmin); err != nil {
		return domain.Hotel{}, err
	}
	h, err := domain.NewHotel(f, s.now())
	if err != nil {
		return domain.Hotel{}, err
	}
	ev, err := hotelEvent(ctx, h)
	if err != nil {
		return domain.Hotel{}, err
	}
	return h, s.Store.CreateHotel(ctx, h, ev)
}

func (s *Service) mutateHotel(ctx context.Context, id uuid.UUID, clientVersion int64, fn func(p auth.Principal, h *domain.Hotel) error) (domain.Hotel, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return domain.Hotel{}, err
	}
	h, err := s.Store.Hotel(ctx, id)
	if err != nil {
		return domain.Hotel{}, err
	}
	if err := versionCheck(&h, clientVersion); err != nil {
		return domain.Hotel{}, err
	}
	if err := fn(p, &h); err != nil {
		return domain.Hotel{}, err
	}
	expected := h.Version
	h.Version++
	ev, err := hotelEvent(ctx, h)
	if err != nil {
		return domain.Hotel{}, err
	}
	if err := s.Store.SaveHotel(ctx, h, expected, ev); err != nil {
		return domain.Hotel{}, err
	}
	return h, nil
}

func (s *Service) UpdateHotel(ctx context.Context, id uuid.UUID, f domain.HotelFields, version int64) (domain.Hotel, error) {
	return s.mutateHotel(ctx, id, version, func(p auth.Principal, h *domain.Hotel) error {
		return h.Update(p, f, s.now())
	})
}

func (s *Service) SetHotelStatus(ctx context.Context, id uuid.UUID, st domain.HotelStatus, version int64) (domain.Hotel, error) {
	return s.mutateHotel(ctx, id, version, func(p auth.Principal, h *domain.Hotel) error {
		return h.SetStatus(p, st, s.now())
	})
}

// AssignManager — синхронно спрашиваем identity, что пользователь —
// менеджер. Это единственный синхронный вызов между сервисами: операция
// редкая (админская), а назначить «клиента» менеджером отеля — ошибка,
// которую дешевле не допустить, чем потом компенсировать.
func (s *Service) AssignManager(ctx context.Context, hotelID, userID uuid.UUID) (domain.Hotel, error) {
	if _, err := auth.RequireRole(ctx, auth.RoleAdmin); err != nil {
		return domain.Hotel{}, err
	}
	role, err := s.Directory.Role(ctx, userID)
	if err != nil {
		return domain.Hotel{}, err
	}
	if role != auth.RoleManager {
		return domain.Hotel{}, apperr.Conflict("not_a_manager", "Сначала выдайте пользователю роль «менеджер отеля»")
	}
	return s.mutateHotel(ctx, hotelID, 0, func(p auth.Principal, h *domain.Hotel) error {
		return h.AssignManager(p, userID, s.now())
	})
}

func (s *Service) RemoveManager(ctx context.Context, hotelID, userID uuid.UUID) (domain.Hotel, error) {
	return s.mutateHotel(ctx, hotelID, 0, func(p auth.Principal, h *domain.Hotel) error {
		return h.RemoveManager(p, userID, s.now())
	})
}

func (s *Service) Hotel(ctx context.Context, id uuid.UUID) (domain.Hotel, []domain.RoomType, error) {
	p, authed := auth.FromContext(ctx)
	h, err := s.Store.Hotel(ctx, id)
	if err != nil {
		return domain.Hotel{}, nil, err
	}
	if !h.CanSee(p, authed) {
		return domain.Hotel{}, nil, apperr.NotFound("Отель")
	}
	rooms, err := s.Store.RoomTypes(ctx, id, authed && h.CanEdit(p))
	if err != nil {
		return domain.Hotel{}, nil, err
	}
	if !(authed && h.CanEdit(p)) {
		h.ManagerIDs = nil // состав менеджеров — внутренняя информация
	}
	return h, rooms, nil
}

func (s *Service) ListHotels(ctx context.Context, f HotelFilter) ([]HotelSummary, int, error) {
	p, authed := auth.FromContext(ctx)
	switch {
	case f.IncludeInactive && authed && p.Role == auth.RoleAdmin:
	case f.IncludeInactive && authed && p.Role == auth.RoleManager:
		f.ManagedBy = p.UserID // менеджер видит свои неактивные, но не чужие
	default:
		f.IncludeInactive = false
	}
	switch f.Sort {
	case "", "name", "-stars", "price", "-price", "-created_at":
	default:
		return nil, 0, apperr.Validation(map[string]string{"sort": "Сортировка: name, -stars, price, -price, -created_at"})
	}
	return s.Store.ListHotels(ctx, f)
}

func (s *Service) ManagedHotels(ctx context.Context, f HotelFilter) ([]HotelSummary, int, error) {
	p, err := auth.RequireRole(ctx, auth.RoleManager, auth.RoleAdmin)
	if err != nil {
		return nil, 0, err
	}
	f.IncludeInactive = true
	if p.Role == auth.RoleManager {
		f.ManagedBy = p.UserID
	}
	return s.Store.ListHotels(ctx, f)
}

func (s *Service) Cities(ctx context.Context) ([]string, error) { return s.Store.Cities(ctx) }

// ---------- категории номеров ----------

func (s *Service) CreateRoomType(ctx context.Context, hotelID uuid.UUID, f domain.RoomFields) (domain.RoomType, error) {
	p, err := auth.RequireRole(ctx, auth.RoleAdmin, auth.RoleManager)
	if err != nil {
		return domain.RoomType{}, err
	}
	h, err := s.Store.Hotel(ctx, hotelID)
	if err != nil {
		return domain.RoomType{}, err
	}
	r, err := domain.NewRoomType(h, p, f, s.now())
	if err != nil {
		return domain.RoomType{}, err
	}
	ev, err := roomEvent(ctx, r)
	if err != nil {
		return domain.RoomType{}, err
	}
	return r, s.Store.CreateRoomType(ctx, r, ev)
}

func (s *Service) mutateRoom(ctx context.Context, id uuid.UUID, clientVersion int64, fn func(p auth.Principal, h domain.Hotel, r *domain.RoomType) error) (domain.RoomType, error) {
	p, err := auth.RequireRole(ctx, auth.RoleAdmin, auth.RoleManager)
	if err != nil {
		return domain.RoomType{}, err
	}
	r, err := s.Store.RoomType(ctx, id)
	if err != nil {
		return domain.RoomType{}, err
	}
	if clientVersion != 0 && clientVersion != r.Version {
		return domain.RoomType{}, apperr.Conflict("version_mismatch", "Категория изменилась в другом окне, обновите страницу")
	}
	h, err := s.Store.Hotel(ctx, r.HotelID)
	if err != nil {
		return domain.RoomType{}, err
	}
	if err := fn(p, h, &r); err != nil {
		return domain.RoomType{}, err
	}
	expected := r.Version
	r.Version++
	ev, err := roomEvent(ctx, r)
	if err != nil {
		return domain.RoomType{}, err
	}
	if err := s.Store.SaveRoomType(ctx, r, expected, ev); err != nil {
		return domain.RoomType{}, err
	}
	return r, nil
}

func (s *Service) UpdateRoomType(ctx context.Context, id uuid.UUID, f domain.RoomFields, version int64) (domain.RoomType, error) {
	return s.mutateRoom(ctx, id, version, func(p auth.Principal, h domain.Hotel, r *domain.RoomType) error {
		return r.Update(h, p, f, s.now())
	})
}

func (s *Service) SetRoomTypeArchived(ctx context.Context, id uuid.UUID, archived bool, version int64) (domain.RoomType, error) {
	return s.mutateRoom(ctx, id, version, func(p auth.Principal, h domain.Hotel, r *domain.RoomType) error {
		return r.SetArchived(h, p, archived, s.now())
	})
}

func (s *Service) RoomType(ctx context.Context, id uuid.UUID) (domain.RoomType, domain.Hotel, error) {
	p, authed := auth.FromContext(ctx)
	r, err := s.Store.RoomType(ctx, id)
	if err != nil {
		return r, domain.Hotel{}, err
	}
	h, err := s.Store.Hotel(ctx, r.HotelID)
	if err != nil {
		return r, h, err
	}
	editor := authed && h.CanEdit(p)
	if !h.CanSee(p, authed) || (r.Status == domain.RoomArchived && !editor) {
		return r, h, apperr.NotFound("Категория номеров")
	}
	if !editor {
		h.ManagerIDs = nil
	}
	return r, h, nil
}
