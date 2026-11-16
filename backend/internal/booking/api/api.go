// Package api — HTTP-адаптер бронирования.
package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
)

type Handler struct {
	Svc *app.Service
	Log *slog.Logger
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/availability", h.availability)
	mux.HandleFunc("POST /api/reservations", h.book)
	mux.HandleFunc("GET /api/reservations/my", h.my)
	mux.HandleFunc("GET /api/reservations", h.hotelReservations)
	mux.HandleFunc("GET /api/reservations/{id}", h.get)
	mux.HandleFunc("POST /api/reservations/{id}/cancel", h.cancel)
	mux.HandleFunc("GET /api/audit", h.audit)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.Error(w, r, h.Log, err)
}

type reservationDTO struct {
	ID           string       `json:"id"`
	HotelID      string       `json:"hotel_id"`
	HotelName    string       `json:"hotel_name"`
	RoomTypeID   string       `json:"room_type_id"`
	RoomTypeName string       `json:"room_type_name"`
	CustomerID   string       `json:"customer_id"`
	CheckIn      string       `json:"check_in"`
	CheckOut     string       `json:"check_out"`
	Nights       int          `json:"nights"`
	Guests       int          `json:"guests"`
	Status       string       `json:"status"`
	Total        domain.Money `json:"total"`
	CreatedAt    time.Time    `json:"created_at"`
	CancelledAt  *time.Time   `json:"cancelled_at,omitempty"`
	CancelReason string       `json:"cancel_reason,omitempty"`
	Version      int64        `json:"version"`
}

func toDTO(r domain.Reservation) reservationDTO {
	return reservationDTO{
		ID: r.ID.String(), HotelID: r.HotelID.String(), HotelName: r.HotelName, RoomTypeID: r.RoomTypeID.String(),
		RoomTypeName: r.RoomTypeName, CustomerID: r.CustomerID.String(), CheckIn: r.Stay.CheckIn.String(),
		CheckOut: r.Stay.CheckOut.String(), Nights: r.Stay.Nights(), Guests: r.Guests, Status: string(r.Status),
		Total: r.Total, CreatedAt: r.CreatedAt, CancelledAt: r.CancelledAt, CancelReason: r.CancelReason, Version: r.Version,
	}
}

func parseDate(field, v string) (domain.Date, error) {
	if v == "" {
		return domain.Date{}, nil
	}
	d, err := domain.ParseDate(v)
	if err != nil {
		return d, apperr.Validation(map[string]string{field: "Дата в формате ГГГГ-ММ-ДД"})
	}
	return d, nil
}

func (h *Handler) availability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hotel, err := uuid.Parse(q.Get("hotel_id"))
	if err != nil {
		h.fail(w, r, apperr.Validation(map[string]string{"hotel_id": "Укажите отель"}))
		return
	}
	in, err := parseDate("check_in", q.Get("check_in"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := parseDate("check_out", q.Get("check_out"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	guests, _ := strconv.Atoi(q.Get("guests"))
	list, err := h.Svc.Availability(r.Context(), hotel, in, out, guests)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) book(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RoomTypeID uuid.UUID `json:"room_type_id"`
		CheckIn    string    `json:"check_in"`
		CheckOut   string    `json:"check_out"`
		Guests     int       `json:"guests"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	ci, err := parseDate("check_in", in.CheckIn)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	co, err := parseDate("check_out", in.CheckOut)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, replayed, err := h.Svc.Book(r.Context(), app.BookInput{
		RoomTypeID: in.RoomTypeID, CheckIn: ci, CheckOut: co, Guests: in.Guests,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	httpx.JSON(w, status, toDTO(res))
}

func listFilter(r *http.Request) (app.ReservationFilter, httpx.Page, error) {
	q := r.URL.Query()
	page := httpx.ParsePage(r, 10, 50)
	f := app.ReservationFilter{Limit: page.PageSize, Offset: page.Offset()}
	switch st := domain.Status(q.Get("status")); st {
	case "", domain.Confirmed, domain.Cancelled:
		f.Status = st
	default:
		return f, page, apperr.Validation(map[string]string{"status": "Статус: confirmed или cancelled"})
	}
	var err error
	if f.From, err = parseDate("from", q.Get("from")); err != nil {
		return f, page, err
	}
	if f.To, err = parseDate("to", q.Get("to")); err != nil {
		return f, page, err
	}
	if v := q.Get("hotel_id"); v != "" {
		if f.HotelID, err = uuid.Parse(v); err != nil {
			return f, page, apperr.Validation(map[string]string{"hotel_id": "Некорректный идентификатор отеля"})
		}
	}
	return f, page, nil
}

func (h *Handler) writeList(w http.ResponseWriter, list []domain.Reservation, total int, page httpx.Page) {
	items := make([]reservationDTO, 0, len(list))
	for _, x := range list {
		items = append(items, toDTO(x))
	}
	httpx.JSON(w, http.StatusOK, httpx.Paged[reservationDTO]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total})
}

func (h *Handler) my(w http.ResponseWriter, r *http.Request) {
	f, page, err := listFilter(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list, total, err := h.Svc.My(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeList(w, list, total, page)
}

func (h *Handler) hotelReservations(w http.ResponseWriter, r *http.Request) {
	f, page, err := listFilter(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list, total, err := h.Svc.HotelReservations(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeList(w, list, total, page)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.Svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDTO(res))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(r, &in); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	res, err := h.Svc.Cancel(r.Context(), id, in.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDTO(res))
}

func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	hotel, err := uuid.Parse(r.URL.Query().Get("hotel_id"))
	if err != nil {
		h.fail(w, r, apperr.Validation(map[string]string{"hotel_id": "Укажите отель"}))
		return
	}
	page := httpx.ParsePage(r, 20, 100)
	list, total, err := h.Svc.AuditLog(r.Context(), hotel, page.PageSize, page.Offset())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, httpx.Paged[app.AuditRecord]{Items: list, Page: page.Page, PageSize: page.PageSize, Total: total})
}
