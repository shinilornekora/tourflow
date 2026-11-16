package api

import (
	"log/slog"
	"net/http"

	"github.com/shinilornekora/tourflow/backend/internal/notification/app"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
)

type Handler struct {
	Svc *app.Service
	Log *slog.Logger
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/notifications", h.list)
	mux.HandleFunc("GET /api/notifications/unread-count", h.unread)
	mux.HandleFunc("POST /api/notifications/{id}/read", h.read)
	mux.HandleFunc("POST /api/notifications/read-all", h.readAll)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.Error(w, r, h.Log, err)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r, 20, 100)
	items, total, err := h.Svc.List(r.Context(), r.URL.Query().Get("unread") == "true", page.PageSize, page.Offset())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, httpx.Paged[app.Notification]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total})
}

func (h *Handler) unread(w http.ResponseWriter, r *http.Request) {
	n, err := h.Svc.Unread(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"unread": n})
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.MarkRead(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.Svc.MarkAllRead(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int64{"marked": n})
}
