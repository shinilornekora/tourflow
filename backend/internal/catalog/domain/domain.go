// Package domain — отель и категория номеров.
//
// Отель — граница арендатора (tenant): менеджер работает только со «своими»
// отелями. Категория номеров (RoomType) — ресурс с вместимостью total_rooms:
// не «номер 305», а «Стандарт двухместный, 12 штук». Конкретный номер
// назначает отель при заселении, системе бронирования это знание не нужно.
package domain

import (
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
)

// Money — value object: сумма в минимальных единицах (копейках), без float.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func NewMoney(amount int64, currency string) (Money, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = "RUB"
	}
	if currency != "RUB" {
		return Money{}, apperr.Validation(map[string]string{"price.currency": "Поддерживается только RUB"})
	}
	if amount <= 0 || amount > 100_000_000_00 {
		return Money{}, apperr.Validation(map[string]string{"price.amount": "Цена должна быть от 0,01 до 100 000 000 ₽"})
	}
	return Money{Amount: amount, Currency: currency}, nil
}

type HotelStatus string

const (
	HotelActive   HotelStatus = "active"
	HotelInactive HotelStatus = "inactive"
)

type HotelFields struct {
	Name        string
	City        string
	Address     string
	Description string
	Stars       int
	PhotoURL    string
}

func (f HotelFields) Normalize() (HotelFields, error) {
	fe := apperr.FieldErrors{}
	f.Name = squash(f.Name)
	f.City = squash(f.City)
	f.Address = squash(f.Address)
	f.Description = strings.TrimSpace(f.Description)
	f.PhotoURL = strings.TrimSpace(f.PhotoURL)
	if n := utf8.RuneCountInString(f.Name); n < 2 || n > 200 {
		fe.Add("name", "Название: 2–200 символов")
	}
	if n := utf8.RuneCountInString(f.City); n < 2 || n > 100 {
		fe.Add("city", "Город: 2–100 символов")
	}
	if utf8.RuneCountInString(f.Address) > 300 {
		fe.Add("address", "Адрес не длиннее 300 символов")
	}
	if utf8.RuneCountInString(f.Description) > 5000 {
		fe.Add("description", "Описание не длиннее 5000 символов")
	}
	if f.Stars < 1 || f.Stars > 5 {
		fe.Add("stars", "Звёздность от 1 до 5")
	}
	if f.PhotoURL != "" && !validURL(f.PhotoURL) {
		fe.Add("photo_url", "Ссылка на фото должна начинаться с http:// или https://")
	}
	return f, fe.Err()
}

type Hotel struct {
	ID uuid.UUID
	HotelFields
	Status     HotelStatus
	ManagerIDs []uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Version    int64
}

func NewHotel(f HotelFields, now time.Time) (Hotel, error) {
	f, err := f.Normalize()
	if err != nil {
		return Hotel{}, err
	}
	return Hotel{ID: uuid.New(), HotelFields: f, Status: HotelActive, CreatedAt: now, UpdatedAt: now, Version: 1}, nil
}

// IsManagedBy — является ли пользователь менеджером отеля.
func (h Hotel) IsManagedBy(user uuid.UUID) bool { return slices.Contains(h.ManagerIDs, user) }

// CanEdit — админ правит любой отель, менеджер — только свой.
func (h Hotel) CanEdit(p auth.Principal) bool {
	return p.Role == auth.RoleAdmin || (p.Role == auth.RoleManager && h.IsManagedBy(p.UserID))
}

// CanSee — неактивный отель виден только админу и его менеджерам.
func (h Hotel) CanSee(p auth.Principal, authenticated bool) bool {
	if h.Status == HotelActive {
		return true
	}
	return authenticated && h.CanEdit(p)
}

func (h *Hotel) Update(p auth.Principal, f HotelFields, now time.Time) error {
	if !h.CanEdit(p) {
		return apperr.Forbidden("Редактировать отель может администратор или его менеджер")
	}
	f, err := f.Normalize()
	if err != nil {
		return err
	}
	h.HotelFields, h.UpdatedAt = f, now
	return nil
}

// SetStatus — включать и выключать отель может только администратор:
// это действие отменяет будущие брони всех клиентов отеля.
func (h *Hotel) SetStatus(p auth.Principal, s HotelStatus, now time.Time) error {
	if p.Role != auth.RoleAdmin {
		return apperr.Forbidden("Менять статус отеля может только администратор")
	}
	if s != HotelActive && s != HotelInactive {
		return apperr.Validation(map[string]string{"status": "Статус: active или inactive"})
	}
	if h.Status == s {
		return apperr.Conflict("status_unchanged", "Отель уже в этом статусе")
	}
	h.Status, h.UpdatedAt = s, now
	return nil
}

func (h *Hotel) AssignManager(p auth.Principal, user uuid.UUID, now time.Time) error {
	if p.Role != auth.RoleAdmin {
		return apperr.Forbidden("Назначать менеджеров может только администратор")
	}
	if h.IsManagedBy(user) {
		return apperr.Conflict("already_manager", "Пользователь уже менеджер этого отеля")
	}
	if len(h.ManagerIDs) >= 20 {
		return apperr.Conflict("too_many_managers", "У отеля не может быть больше 20 менеджеров")
	}
	h.ManagerIDs = append(h.ManagerIDs, user)
	h.UpdatedAt = now
	return nil
}

func (h *Hotel) RemoveManager(p auth.Principal, user uuid.UUID, now time.Time) error {
	if p.Role != auth.RoleAdmin {
		return apperr.Forbidden("Снимать менеджеров может только администратор")
	}
	i := slices.Index(h.ManagerIDs, user)
	if i < 0 {
		return apperr.NotFound("Менеджер отеля")
	}
	h.ManagerIDs = slices.Delete(h.ManagerIDs, i, i+1)
	h.UpdatedAt = now
	return nil
}

// ---------- категория номеров ----------

type RoomStatus string

const (
	RoomActive   RoomStatus = "active"
	RoomArchived RoomStatus = "archived"
)

type RoomFields struct {
	Name        string
	Description string
	Capacity    int
	TotalRooms  int
	Price       Money
	Amenities   []string
	PhotoURL    string
}

func (f RoomFields) Normalize() (RoomFields, error) {
	fe := apperr.FieldErrors{}
	f.Name = squash(f.Name)
	f.Description = strings.TrimSpace(f.Description)
	f.PhotoURL = strings.TrimSpace(f.PhotoURL)
	if n := utf8.RuneCountInString(f.Name); n < 2 || n > 120 {
		fe.Add("name", "Название: 2–120 символов")
	}
	if f.Capacity < 1 || f.Capacity > 10 {
		fe.Add("capacity", "Вместимость: от 1 до 10 гостей")
	}
	if f.TotalRooms < 1 || f.TotalRooms > 1000 {
		fe.Add("total_rooms", "Количество номеров: от 1 до 1000")
	}
	if m, err := NewMoney(f.Price.Amount, f.Price.Currency); err != nil {
		for k, v := range apperr.As(err).Fields {
			fe.Add(k, v)
		}
	} else {
		f.Price = m
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(f.Amenities))
	for _, a := range f.Amenities {
		a = squash(a)
		if a == "" || seen[strings.ToLower(a)] {
			continue
		}
		if utf8.RuneCountInString(a) > 40 {
			fe.Add("amenities", "Удобство не длиннее 40 символов")
			continue
		}
		seen[strings.ToLower(a)] = true
		clean = append(clean, a)
	}
	if len(clean) > 30 {
		fe.Add("amenities", "Не больше 30 удобств")
	}
	f.Amenities = clean
	if f.PhotoURL != "" && !validURL(f.PhotoURL) {
		fe.Add("photo_url", "Ссылка на фото должна начинаться с http:// или https://")
	}
	return f, fe.Err()
}

type RoomType struct {
	ID      uuid.UUID
	HotelID uuid.UUID
	RoomFields
	Status    RoomStatus
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int64
}

func NewRoomType(h Hotel, p auth.Principal, f RoomFields, now time.Time) (RoomType, error) {
	if !h.CanEdit(p) {
		return RoomType{}, apperr.Forbidden("Добавлять номера может администратор или менеджер отеля")
	}
	f, err := f.Normalize()
	if err != nil {
		return RoomType{}, err
	}
	return RoomType{ID: uuid.New(), HotelID: h.ID, RoomFields: f, Status: RoomActive, CreatedAt: now, UpdatedAt: now, Version: 1}, nil
}

func (r *RoomType) Update(h Hotel, p auth.Principal, f RoomFields, now time.Time) error {
	if !h.CanEdit(p) {
		return apperr.Forbidden("Редактировать номера может администратор или менеджер отеля")
	}
	if r.Status == RoomArchived {
		return apperr.Conflict("room_archived", "Категория в архиве, сначала восстановите её")
	}
	f, err := f.Normalize()
	if err != nil {
		return err
	}
	r.RoomFields, r.UpdatedAt = f, now
	return nil
}

// SetArchived — категория не удаляется физически: на неё ссылаются брони
// в booking. Архивная категория просто перестаёт продаваться.
func (r *RoomType) SetArchived(h Hotel, p auth.Principal, archived bool, now time.Time) error {
	if !h.CanEdit(p) {
		return apperr.Forbidden("Архивировать номера может администратор или менеджер отеля")
	}
	next := RoomActive
	if archived {
		next = RoomArchived
	}
	if r.Status == next {
		return apperr.Conflict("status_unchanged", "Категория уже в этом статусе")
	}
	r.Status, r.UpdatedAt = next, now
	return nil
}

// ---------- события ----------

// HotelSnapshot — событие с полным состоянием отеля (event-carried state
// transfer): потребитель хранит копию без обратных вызовов в catalog.
type HotelSnapshot struct {
	HotelID    uuid.UUID   `json:"hotel_id"`
	Name       string      `json:"name"`
	City       string      `json:"city"`
	Stars      int         `json:"stars"`
	Status     HotelStatus `json:"status"`
	ManagerIDs []uuid.UUID `json:"manager_ids"`
	Version    int64       `json:"version"`
	// ChangedBy — кто выполнил изменение (администратор или менеджер).
	// У каталога своего журнала аудита нет; автор уходит в событии, и
	// booking записывает его в аудит отеля при отключении (T124).
	ChangedBy *uuid.UUID `json:"changed_by,omitempty"`
}

func (h Hotel) Snapshot() HotelSnapshot {
	ids := h.ManagerIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return HotelSnapshot{HotelID: h.ID, Name: h.Name, City: h.City, Stars: h.Stars, Status: h.Status, ManagerIDs: ids, Version: h.Version}
}

type RoomTypeSnapshot struct {
	RoomTypeID uuid.UUID  `json:"room_type_id"`
	HotelID    uuid.UUID  `json:"hotel_id"`
	Name       string     `json:"name"`
	Capacity   int        `json:"capacity"`
	TotalRooms int        `json:"total_rooms"`
	Price      Money      `json:"price"`
	Status     RoomStatus `json:"status"`
	Version    int64      `json:"version"`
}

func (r RoomType) Snapshot() RoomTypeSnapshot {
	return RoomTypeSnapshot{RoomTypeID: r.ID, HotelID: r.HotelID, Name: r.Name, Capacity: r.Capacity,
		TotalRooms: r.TotalRooms, Price: r.Price, Status: r.Status, Version: r.Version}
}

const (
	EventHotelUpserted    = "HotelUpserted"
	EventRoomTypeUpserted = "RoomTypeUpserted"
)

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(s) <= 1000
}
