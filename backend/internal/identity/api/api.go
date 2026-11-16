// Package api — HTTP-адаптер identity.
package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shinilornekora/tourflow/backend/internal/identity/app"
	"github.com/shinilornekora/tourflow/backend/internal/identity/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
)

const refreshCookie = "tf_refresh"

type Handler struct {
	Svc          *app.Service
	Log          *slog.Logger
	SecureCookie bool
	// InternalToken защищает /internal/*: эти маршруты не публикуются через
	// gateway, но и внутри сети не должны быть открыты всем.
	InternalToken string
}

type userDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	FullName  string    `json:"full_name"`
	Phone     string    `json:"phone"`
	Role      auth.Role `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	Version   int64     `json:"version"`
}

func toDTO(u domain.User) userDTO {
	return userDTO{ID: u.ID.String(), Email: u.Email.String(), FullName: u.Profile.FullName, Phone: u.Profile.Phone,
		Role: u.Role, CreatedAt: u.CreatedAt, Version: u.Version}
}

type sessionDTO struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        userDTO   `json:"user"`
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/register", h.register)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/refresh", h.refresh)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.HandleFunc("GET /api/users/me", h.me)
	mux.HandleFunc("PATCH /api/users/me", h.updateMe)
	mux.HandleFunc("POST /api/users/me/password", h.changePassword)
	mux.HandleFunc("GET /api/users", h.list)
	mux.HandleFunc("PATCH /api/users/{id}/role", h.changeRole)
	mux.HandleFunc("GET /internal/users/{id}", h.internalUser)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.Error(w, r, h.Log, err)
}

func (h *Handler) writeSession(w http.ResponseWriter, s app.Session, status int) {
	// Refresh-токен — только в HttpOnly-cookie: JavaScript (а значит и XSS)
	// его не прочитает. Path ограничен /api/auth, cookie не уходит с
	// остальными запросами. Access-токен живёт в памяти SPA.
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookie, Value: s.RefreshToken, Path: "/api/auth",
		Expires: s.RefreshExpiresAt, HttpOnly: true, Secure: h.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
	httpx.JSON(w, status, sessionDTO{AccessToken: s.AccessToken, ExpiresAt: s.AccessExpiresAt, User: toDTO(s.User)})
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: refreshCookie, Value: "", Path: "/api/auth", MaxAge: -1,
		HttpOnly: true, Secure: h.SecureCookie, SameSite: http.SameSiteStrictMode})
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
		Phone    string `json:"phone"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := h.Svc.Register(r.Context(), app.RegisterInput{Email: in.Email, Password: in.Password, FullName: in.FullName, Phone: in.Phone})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeSession(w, s, http.StatusCreated)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := h.Svc.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeSession(w, s, http.StatusOK)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(refreshCookie)
	if err != nil || c.Value == "" {
		h.clearCookie(w)
		h.fail(w, r, authErr())
		return
	}
	s, err := h.Svc.Refresh(r.Context(), c.Value)
	if err != nil {
		h.clearCookie(w)
		h.fail(w, r, err)
		return
	}
	h.writeSession(w, s, http.StatusOK)
}

func authErr() error {
	return apperr.Unauthorized("Сессия истекла, войдите заново")
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(refreshCookie); err == nil {
		_ = h.Svc.Logout(r.Context(), c.Value)
	}
	h.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, err := h.Svc.Me(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDTO(u))
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FullName string `json:"full_name"`
		Phone    string `json:"phone"`
		Version  int64  `json:"version"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.Svc.UpdateMe(r.Context(), in.FullName, in.Phone, in.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDTO(u))
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.ChangePassword(r.Context(), in.Current, in.New); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page := httpx.ParsePage(r, 20, 100)
	q := r.URL.Query()
	users, total, err := h.Svc.ListUsers(r.Context(), app.UserFilter{
		Role: auth.Role(q.Get("role")), Query: strings.TrimSpace(q.Get("q")), Limit: page.PageSize, Offset: page.Offset(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]userDTO, 0, len(users))
	for _, u := range users {
		items = append(items, toDTO(u))
	}
	httpx.JSON(w, http.StatusOK, httpx.Paged[userDTO]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total})
}

func (h *Handler) changeRole(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in struct {
		Role auth.Role `json:"role"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.Svc.ChangeRole(r.Context(), id, in.Role)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDTO(u))
}

func (h *Handler) internalUser(w http.ResponseWriter, r *http.Request) {
	if h.InternalToken == "" || r.Header.Get("X-Internal-Token") != h.InternalToken {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.Svc.UserByID(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"id": u.ID, "role": u.Role, "full_name": u.Profile.FullName, "email": u.Email.String()})
}
