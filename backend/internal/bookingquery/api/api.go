// Package api — HTTP-адаптер модели чтения броней. Только GET: команды
// (создать, отменить) принимает booking.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/app"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
)

type Handler struct {
	Svc *app.Service
	Log *slog.Logger
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/reservations/my", h.my)
	mux.HandleFunc("GET /api/reservations/my/summary", h.mySummary)
	mux.HandleFunc("GET /api/reservations/summary", h.hotelSummary)
	mux.HandleFunc("GET /api/reservations", h.hotelReservations)
	mux.HandleFunc("GET /api/reservations/{id}", h.get)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.Error(w, r, h.Log, err)
}

func parseDate(field, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	d, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return d, apperr.Validation(map[string]string{field: "Дата в формате ГГГГ-ММ-ДД"})
	}
	return d, nil
}

// filter разбирает общие параметры: status, from, to, hotel_id, page, page_size.
func filter(r *http.Request) (app.Filter, httpx.Page, error) {
	q := r.URL.Query()
	page := httpx.ParsePage(r, 10, 50)
	f := app.Filter{Limit: page.PageSize, Offset: page.Offset()}
	switch st := q.Get("status"); st {
	case "", app.StatusConfirmed, app.StatusCancelled:
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

func (h *Handler) list(w http.ResponseWriter, r *http.Request, run func(app.Filter) ([]app.Card, int, error)) {
	f, page, err := filter(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, total, err := run(f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, httpx.Paged[app.Card]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total})
}

func (h *Handler) my(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, func(f app.Filter) ([]app.Card, int, error) { return h.Svc.My(r.Context(), f) })
}

func (h *Handler) hotelReservations(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, func(f app.Filter) ([]app.Card, int, error) { return h.Svc.HotelReservations(r.Context(), f) })
}

func (h *Handler) mySummary(w http.ResponseWriter, r *http.Request) {
	s, err := h.Svc.MySummary(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

func (h *Handler) hotelSummary(w http.ResponseWriter, r *http.Request) {
	f, _, err := filter(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := h.Svc.HotelSummary(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.Svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}
