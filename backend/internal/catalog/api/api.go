// Package api — HTTP-адаптер каталога.
package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/catalog/app"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
)

type Handler struct {
	Svc *app.Service
	Log *slog.Logger
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hotels", h.listHotels)
	mux.HandleFunc("GET /api/hotels/managed", h.managedHotels)
	mux.HandleFunc("POST /api/hotels", h.createHotel)
	mux.HandleFunc("GET /api/hotels/{id}", h.getHotel)
	mux.HandleFunc("PUT /api/hotels/{id}", h.updateHotel)
	mux.HandleFunc("PATCH /api/hotels/{id}/status", h.setHotelStatus)
	mux.HandleFunc("POST /api/hotels/{id}/managers", h.assignManager)
	mux.HandleFunc("DELETE /api/hotels/{id}/managers/{userID}", h.removeManager)
	mux.HandleFunc("POST /api/hotels/{id}/room-types", h.createRoomType)
	mux.HandleFunc("GET /api/room-types/{id}", h.getRoomType)
	mux.HandleFunc("PUT /api/room-types/{id}", h.updateRoomType)
	mux.HandleFunc("PATCH /api/room-types/{id}/status", h.setRoomStatus)
	mux.HandleFunc("GET /api/cities", h.cities)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.Error(w, r, h.Log, err)
}

// ---------- DTO ----------

type hotelDTO struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	City          string        `json:"city"`
	Address       string        `json:"address"`
	Description   string        `json:"description"`
	Stars         int           `json:"stars"`
	PhotoURL      string        `json:"photo_url"`
	Status        string        `json:"status"`
	ManagerIDs    []uuid.UUID   `json:"manager_ids,omitempty"`
	MinPrice      *domain.Money `json:"min_price,omitempty"`
	RoomTypeCount *int          `json:"room_type_count,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Version       int64         `json:"version"`
}

func toHotel(x domain.Hotel) hotelDTO {
	return hotelDTO{ID: x.ID.String(), Name: x.Name, City: x.City, Address: x.Address, Description: x.Description,
		Stars: x.Stars, PhotoURL: x.PhotoURL, Status: string(x.Status), ManagerIDs: x.ManagerIDs,
		CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, Version: x.Version}
}

type roomDTO struct {
	ID          string       `json:"id"`
	HotelID     string       `json:"hotel_id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Capacity    int          `json:"capacity"`
	TotalRooms  int          `json:"total_rooms"`
	Price       domain.Money `json:"price"`
	Amenities   []string     `json:"amenities"`
	PhotoURL    string       `json:"photo_url"`
	Status      string       `json:"status"`
	Version     int64        `json:"version"`
}

func toRoom(r domain.RoomType) roomDTO {
	am := r.Amenities
	if am == nil {
		am = []string{}
	}
	return roomDTO{ID: r.ID.String(), HotelID: r.HotelID.String(), Name: r.Name, Description: r.Description,
		Capacity: r.Capacity, TotalRooms: r.TotalRooms, Price: r.Price, Amenities: am, PhotoURL: r.PhotoURL,
		Status: string(r.Status), Version: r.Version}
}

type hotelInput struct {
	Name        string `json:"name"`
	City        string `json:"city"`
	Address     string `json:"address"`
	Description string `json:"description"`
	Stars       int    `json:"stars"`
	PhotoURL    string `json:"photo_url"`
	Version     int64  `json:"version"`
}

func (in hotelInput) fields() domain.HotelFields {
	return domain.HotelFields{Name: in.Name, City: in.City, Address: in.Address, Description: in.Description, Stars: in.Stars, PhotoURL: in.PhotoURL}
}

type roomInput struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Capacity    int          `json:"capacity"`
	TotalRooms  int          `json:"total_rooms"`
	Price       domain.Money `json:"price"`
	Amenities   []string     `json:"amenities"`
	PhotoURL    string       `json:"photo_url"`
	Version     int64        `json:"version"`
}

func (in roomInput) fields() domain.RoomFields {
	return domain.RoomFields{Name: in.Name, Description: in.Description, Capacity: in.Capacity, TotalRooms: in.TotalRooms,
		Price: in.Price, Amenities: in.Amenities, PhotoURL: in.PhotoURL}
}

// ---------- отели ----------

func parseFilter(r *http.Request) (app.HotelFilter, httpx.Page) {
	q := r.URL.Query()
	page := httpx.ParsePage(r, 12, 50)
	stars, _ := strconv.Atoi(q.Get("stars_min"))
	guests, _ := strconv.Atoi(q.Get("guests"))
	priceMax, _ := strconv.ParseInt(q.Get("price_max"), 10, 64)
	return app.HotelFilter{
		Query: strings.TrimSpace(q.Get("q")), City: q.Get("city"), StarsMin: stars, Guests: guests, PriceMax: priceMax,
		Sort: q.Get("sort"), IncludeInactive: q.Get("include_inactive") == "true",
		Limit: page.PageSize, Offset: page.Offset(),
	}, page
}

func (h *Handler) writeList(w http.ResponseWriter, list []app.HotelSummary, total int, page httpx.Page) {
	items := make([]hotelDTO, 0, len(list))
	for _, s := range list {
		d := toHotel(s.Hotel)
		d.ManagerIDs = nil
		d.MinPrice = s.MinPrice
		cnt := s.RoomTypeCount
		d.RoomTypeCount = &cnt
		items = append(items, d)
	}
	httpx.JSON(w, http.StatusOK, httpx.Paged[hotelDTO]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total})
}

func (h *Handler) listHotels(w http.ResponseWriter, r *http.Request) {
	f, page := parseFilter(r)
	list, total, err := h.Svc.ListHotels(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeList(w, list, total, page)
}

func (h *Handler) managedHotels(w http.ResponseWriter, r *http.Request) {
	f, page := parseFilter(r)
	list, total, err := h.Svc.ManagedHotels(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeList(w, list, total, page)
}

func (h *Handler) createHotel(w http.ResponseWriter, r *http.Request) {
	var in hotelInput
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	x, err := h.Svc.CreateHotel(r.Context(), in.fields())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toHotel(x))
}

func (h *Handler) getHotel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	x, rooms, err := h.Svc.Hotel(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]roomDTO, 0, len(rooms))
	for _, rt := range rooms {
		out = append(out, toRoom(rt))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"hotel": toHotel(x), "room_types": out})
}

func (h *Handler) updateHotel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in hotelInput
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	x, err := h.Svc.UpdateHotel(r.Context(), id, in.fields(), in.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toHotel(x))
}

func (h *Handler) setHotelStatus(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	x, err := h.Svc.SetHotelStatus(r.Context(), id, domain.HotelStatus(in.Status), in.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toHotel(x))
}

func (h *Handler) assignManager(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in struct {
		UserID uuid.UUID `json:"user_id"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	x, err := h.Svc.AssignManager(r.Context(), id, in.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toHotel(x))
}

func (h *Handler) removeManager(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	userID, err := httpx.PathUUID(r, "userID")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	x, err := h.Svc.RemoveManager(r.Context(), id, userID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toHotel(x))
}

func (h *Handler) cities(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.Cities(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
}

// ---------- категории ----------

func (h *Handler) createRoomType(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in roomInput
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	rt, err := h.Svc.CreateRoomType(r.Context(), id, in.fields())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toRoom(rt))
}

func (h *Handler) getRoomType(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rt, hotel, err := h.Svc.RoomType(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"room_type": toRoom(rt), "hotel": toHotel(hotel)})
}

func (h *Handler) updateRoomType(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in roomInput
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	rt, err := h.Svc.UpdateRoomType(r.Context(), id, in.fields(), in.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toRoom(rt))
}

func (h *Handler) setRoomStatus(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	rt, err := h.Svc.SetRoomTypeArchived(r.Context(), id, in.Status == string(domain.RoomArchived), in.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toRoom(rt))
}
