// Package httpx — HTTP-обвязка всех сервисов: запуск с graceful shutdown
// и пробами (из orderflow, ПКП-1), JSON-ответы, единый формат ошибок,
// middleware для request id, восстановления после паники и журнала запросов.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/logx"
)

const RequestIDHeader = "X-Request-ID"

// MaxBodyBytes ограничивает тело запроса: медленная или огромная загрузка
// не должна занимать воркер бесконечно (урок ПКП-1, «медленная загрузка тела»).
const MaxBodyBytes = 1 << 20

// Probes хранит состояние готовности. При остановке readyz переводится в 503
// раньше, чем сервер перестаёт принимать соединения, — балансировщик успевает
// убрать инстанс из ротации.
type Probes struct {
	ready atomic.Bool
	check func(context.Context) error
}

// NewProbes создаёт пробы; check (например, ping БД) может быть nil.
func NewProbes(check func(context.Context) error) *Probes {
	p := &Probes{check: check}
	p.ready.Store(true)
	return p
}

func (p *Probes) SetReady(v bool) { p.ready.Store(v) }

func (p *Probes) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !p.ready.Load() {
			JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
			return
		}
		if p.check != nil {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if err := p.check(ctx); err != nil {
				JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "dependency_down"})
				return
			}
		}
		JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
}

// Run запускает сервер и останавливает его по ctx.Done(): сначала readyz=503,
// затем пауза drainDelay, затем Shutdown с бюджетом timeout.
func Run(ctx context.Context, log *slog.Logger, addr string, h http.Handler, probes *Probes, timeout time.Duration) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server started", "address", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	if probes != nil {
		probes.SetReady(false)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown http: %w", err)
	}
	log.Info("http server stopped")
	return nil
}

// ---------- ответы ----------

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

// StatusOf переводит вид прикладной ошибки в HTTP-статус.
func StatusOf(kind apperr.Kind) int {
	switch kind {
	case apperr.KindValidation:
		return http.StatusUnprocessableEntity
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Error пишет ошибку в едином формате. Внутренние детали в ответ не попадают,
// только в журнал.
func Error(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	e := apperr.As(err)
	status := StatusOf(e.Kind)
	if status >= 500 {
		log.ErrorContext(r.Context(), "request failed", "error", err, "path", r.URL.Path)
	}
	JSON(w, status, errorBody{Error: errorPayload{
		Code:      e.Code,
		Message:   e.Message,
		Fields:    e.Fields,
		RequestID: logx.RequestID(r.Context()),
	}})
}

// Decode читает JSON-тело с ограничением размера и запретом неизвестных полей.
func Decode(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return apperr.New(apperr.KindValidation, "body_too_large", "Слишком большой запрос")
		case errors.Is(err, io.EOF):
			return apperr.New(apperr.KindValidation, "empty_body", "Пустое тело запроса")
		default:
			return apperr.New(apperr.KindValidation, "bad_json", "Некорректный JSON: "+err.Error())
		}
	}
	return nil
}

// ---------- пагинация ----------

type Page struct {
	Page     int
	PageSize int
}

func (p Page) Offset() int { return (p.Page - 1) * p.PageSize }

// ParsePage читает page/page_size; page_size ограничен maxSize.
func ParsePage(r *http.Request, defSize, maxSize int) Page {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("page_size"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = defSize
	}
	if size > maxSize {
		size = maxSize
	}
	return Page{Page: page, PageSize: size}
}

// Paged — конверт для списков.
type Paged[T any] struct {
	Items    []T `json:"items"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Total    int `json:"total"`
}

// ---------- middleware ----------

type Middleware func(http.Handler) http.Handler

func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RequestID берёт X-Request-ID из входящего запроса или создаёт новый.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
			if id == "" || len(id) > 128 {
				id = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, id)
			r.Header.Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(logx.WithRequestID(r.Context(), id)))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AccessLog пишет одну строку на запрос: метод, путь, статус, длительность.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}
			started := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			log.InfoContext(r.Context(), "http request completed",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(started).Milliseconds(),
				"request_id", logx.RequestID(r.Context()),
			)
		})
	}
}

// Recover превращает панику обработчика в 500 вместо падения процесса.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.ErrorContext(r.Context(), "panic in handler", "panic", fmt.Sprint(v), "path", r.URL.Path)
					Error(w, r, log, fmt.Errorf("panic: %v", v))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Standard — стандартный набор middleware для внутренних сервисов.
func Standard(log *slog.Logger) []Middleware {
	return []Middleware{RequestID(), Recover(log), AccessLog(log)}
}

// PathUUID разбирает параметр пути как UUID.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, apperr.New(apperr.KindValidation, "bad_id", "Некорректный идентификатор "+name)
	}
	return id, nil
}
