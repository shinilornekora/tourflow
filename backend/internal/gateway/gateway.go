// Package gateway — единая точка входа для SPA.
//
// Что делает gateway:
//   - маршрутизирует /api/* в нужный сервис по префиксу;
//   - отклоняет запросы с невалидным токеном до того, как они дойдут до
//     сервисов (сервисы всё равно проверяют токен сами — zero trust);
//   - ограничивает частоту запросов: общий лимит на IP и жёсткий лимит
//     на вход/регистрацию против перебора паролей;
//   - изолирует отказы: у каждого upstream свой circuit breaker, падение
//     notification не тянет за собой каталог;
//   - не пропускает наружу /internal/*.
//
// Чего gateway НЕ делает: бизнес-логики и агрегации ответов. Это не BFF —
// SPA ходит в ресурсы сервисов напрямую через единый адрес.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/resilience"
)

type Upstream struct {
	Name     string
	URL      *url.URL
	Prefixes []string
	Timeout  time.Duration
	Breaker  *resilience.Breaker
	proxy    *httputil.ReverseProxy
}

type Gateway struct {
	Log         *slog.Logger
	Verifier    *auth.Verifier
	Upstreams   []*Upstream
	AllowOrigin []string
	GlobalLimit *resilience.TokenBucket
	AuthLimit   *resilience.TokenBucket
	TrustProxy  bool
}

type breakerTransport struct {
	next http.RoundTripper
	b    *resilience.Breaker
}

func (t breakerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.b.Allow(); err != nil {
		return nil, err
	}
	resp, err := t.next.RoundTrip(r)
	// 5xx и сетевые ошибки — отказ зависимости; 4xx — нормальный ответ.
	t.b.Done(err == nil && resp.StatusCode < 500)
	return resp, err
}

// Init создаёт прокси для каждого upstream.
func (g *Gateway) Init() {
	base := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	for _, u := range g.Upstreams {
		u := u
		if u.Breaker == nil {
			u.Breaker = resilience.NewBreaker(5, 10*time.Second)
		}
		u.Breaker.OnChange = func(from, to resilience.State) {
			g.Log.Warn("circuit breaker state changed", "upstream", u.Name, "from", from.String(), "to", to.String())
		}
		p := httputil.NewSingleHostReverseProxy(u.URL)
		p.Transport = breakerTransport{next: base, b: u.Breaker}
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			var e error
			switch {
			case errors.Is(err, resilience.ErrOpen):
				e = apperr.Unavailable("upstream_circuit_open", "Сервис временно недоступен, попробуйте через несколько секунд", err)
			case errors.Is(err, context.DeadlineExceeded):
				e = apperr.New(apperr.KindUnavailable, "upstream_timeout", "Сервис не ответил вовремя")
			case errors.Is(err, context.Canceled):
				return // клиент ушёл, отвечать некому
			default:
				e = apperr.Unavailable("upstream_unavailable", "Сервис временно недоступен", err)
			}
			g.Log.Warn("proxy error", "upstream", u.Name, "error", err, "path", r.URL.Path)
			httpx.Error(w, r, g.Log, e)
		}
		u.proxy = p
	}
}

func (g *Gateway) route(path string) *Upstream {
	var best *Upstream
	bestLen := -1
	for _, u := range g.Upstreams {
		for _, p := range u.Prefixes {
			if (path == p || strings.HasPrefix(path, p+"/") || strings.HasPrefix(path, p+"?")) && len(p) > bestLen {
				best, bestLen = u, len(p)
			}
		}
	}
	return best
}

func (g *Gateway) clientIP(r *http.Request) string {
	if g.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Берём последний адрес: его дописал доверенный прокси. Первые
			// элементы списка присылает клиент, и верить им нельзя.
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.securityHeaders(w)
	if g.cors(w, r) {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/internal/") || strings.Contains(r.URL.Path, "/internal/") {
		httpx.Error(w, r, g.Log, apperr.NotFound("Ресурс"))
		return
	}
	ip := g.clientIP(r)
	if g.GlobalLimit != nil && !g.GlobalLimit.Allow(ip) {
		g.tooMany(w, r)
		return
	}
	if g.AuthLimit != nil && r.Method == http.MethodPost &&
		(r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/register") && !g.AuthLimit.Allow(ip) {
		g.tooMany(w, r)
		return
	}

	up := g.route(r.URL.Path)
	if up == nil {
		httpx.Error(w, r, g.Log, apperr.NotFound("Маршрут"))
		return
	}

	// Ранняя проверка токена: мусорный или просроченный токен не тратит
	// ресурсы сервисов. Отсутствие токена не ошибка — публичные маршруты.
	if raw := auth.BearerToken(r); raw != "" {
		if _, err := g.Verifier.Verify(r.Context(), raw); err != nil {
			httpx.Error(w, r, g.Log, err)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), up.Timeout)
	defer cancel()
	r = r.WithContext(ctx)
	r.Header.Set("X-Forwarded-For", ip)
	r.Header.Del("X-Internal-Token") // клиент не может выдать себя за сервис
	up.proxy.ServeHTTP(w, r)
}

func (g *Gateway) tooMany(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "1")
	httpx.JSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{
		"code": "rate_limited", "message": "Слишком много запросов, подождите немного",
	}})
}

func (g *Gateway) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
}

// cors отвечает на preflight. В основной схеме SPA и API на одном origin
// (nginx или dev-server проксируют /api), и CORS не нужен вовсе; allowlist
// оставлен для запуска фронтенда с другого адреса.
func (g *Gateway) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	allowed := false
	for _, o := range g.AllowOrigin {
		if o == origin {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}
