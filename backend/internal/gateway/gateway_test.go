package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/resilience"
)

func newGateway(t *testing.T, catalog, booking http.Handler) (*Gateway, *auth.Signer) {
	t.Helper()
	signer, _ := auth.NewSigner("", time.Minute)
	cs, bs := httptest.NewServer(catalog), httptest.NewServer(booking)
	t.Cleanup(cs.Close)
	t.Cleanup(bs.Close)
	cu, _ := url.Parse(cs.URL)
	bu, _ := url.Parse(bs.URL)
	g := &Gateway{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verifier: auth.NewVerifier(auth.StaticKeys(signer.PublicKeys())),
		Upstreams: []*Upstream{
			{Name: "catalog", URL: cu, Prefixes: []string{"/api/hotels"}, Timeout: time.Second, Breaker: resilience.NewBreaker(2, time.Minute)},
			{Name: "booking", URL: bu, Prefixes: []string{"/api/reservations"}, Timeout: 200 * time.Millisecond},
		},
		AuthLimit: resilience.NewTokenBucket(0.0001, 2),
	}
	g.Init()
	return g, signer
}

func do(g http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Internal-Token", "forged")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestRoutingAuthAndIsolation(t *testing.T) {
	var catalogCalls atomic.Int32
	var sawForged atomic.Bool
	catalog := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogCalls.Add(1)
		if r.Header.Get("X-Internal-Token") != "" {
			sawForged.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	})
	booking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	})
	g, signer := newGateway(t, catalog, booking)

	if w := do(g, "GET", "/api/hotels?city=x", ""); w.Code != 200 {
		t.Fatalf("public route: %d", w.Code)
	}
	if sawForged.Load() {
		t.Fatal("client-supplied X-Internal-Token must be stripped")
	}
	if w := do(g, "GET", "/api/hotels", "garbage"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token rejected at the edge: %d", w.Code)
	}
	tok, _, _ := signer.Issue(uuid.New(), auth.RoleCustomer)
	if w := do(g, "GET", "/api/hotels/1", tok); w.Code != 200 {
		t.Fatalf("valid token passes: %d", w.Code)
	}
	if w := do(g, "GET", "/api/unknown", ""); w.Code != 404 {
		t.Fatalf("unknown prefix: %d", w.Code)
	}
	if w := do(g, "GET", "/internal/users/1", ""); w.Code != 404 {
		t.Fatalf("internal routes are not exposed: %d", w.Code)
	}
	if w := do(g, "GET", "/api/hotelsXYZ", ""); w.Code != 404 {
		t.Fatalf("prefix must match on segment boundary: %d", w.Code)
	}
	// Медленный booking упирается в бюджет и не влияет на catalog.
	if w := do(g, "GET", "/api/reservations/my", tok); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("slow upstream → 503 by timeout, got %d", w.Code)
	}
	before := catalogCalls.Load()
	if w := do(g, "GET", "/api/hotels", ""); w.Code != 200 || catalogCalls.Load() != before+1 {
		t.Fatal("catalog must keep working while booking is slow")
	}
}

func TestBreakerOpensOnUpstream5xx(t *testing.T) {
	var calls atomic.Int32
	catalog := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	g, _ := newGateway(t, catalog, http.NotFoundHandler())
	for i := 0; i < 2; i++ {
		do(g, "GET", "/api/hotels", "")
	}
	w := do(g, "GET", "/api/hotels", "")
	if w.Code != http.StatusServiceUnavailable || calls.Load() != 2 {
		t.Fatalf("after 2 failures breaker must short-circuit: code=%d calls=%d", w.Code, calls.Load())
	}
}

func TestLoginRateLimit(t *testing.T) {
	g, _ := newGateway(t, http.NotFoundHandler(), http.NotFoundHandler())
	g.Upstreams[0].Prefixes = append(g.Upstreams[0].Prefixes, "/api/auth")
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, do(g, "POST", "/api/auth/login", "").Code)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Fatalf("3rd login in a burst of 2 must be limited: %v", codes)
	}
}

// Лимит попыток входа считается по адресу клиента. Подменой X-Forwarded-For
// его не обойти: без TRUST_PROXY заголовок игнорируется, а с ним берётся
// последний адрес списка — его дописал доверенный прокси, а не клиент.
func TestLoginRateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	login := func(g *Gateway, xff string) int {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}
	for _, trust := range []bool{false, true} {
		g, _ := newGateway(t, http.NotFoundHandler(), http.NotFoundHandler())
		g.Upstreams[0].Prefixes = append(g.Upstreams[0].Prefixes, "/api/auth")
		g.TrustProxy = trust
		// Клиент каждый раз называется новым адресом; прокси дописывает настоящий.
		var codes []int
		for i := 0; i < 3; i++ {
			xff := fmt.Sprintf("10.0.0.%d", i+1)
			if trust {
				xff += ", 203.0.113.7"
			}
			codes = append(codes, login(g, xff))
		}
		if codes[2] != http.StatusTooManyRequests {
			t.Errorf("trustProxy=%v: spoofed X-Forwarded-For bypassed the login limit: %v", trust, codes)
		}
		// Контроль: за доверенным прокси другой клиент — другой счётчик.
		if trust {
			if code := login(g, "10.0.0.1, 198.51.100.9"); code == http.StatusTooManyRequests {
				t.Errorf("another client behind the proxy must have its own bucket, got %d", code)
			}
		}
	}
}

// CQRS: чтение броней уходит в booking-query, команды — в booking. Префикс
// один и тот же, различает их метод.
func TestReadsAndCommandsSplitByMethod(t *testing.T) {
	hit := func(name string, got *atomic.Value) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got.Store(name + " " + r.Method + " " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		})
	}
	var got atomic.Value
	bs, qs := httptest.NewServer(hit("booking", &got)), httptest.NewServer(hit("query", &got))
	t.Cleanup(bs.Close)
	t.Cleanup(qs.Close)
	bu, _ := url.Parse(bs.URL)
	qu, _ := url.Parse(qs.URL)
	g := &Gateway{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verifier: auth.NewVerifier(auth.StaticKeys(nil)),
		Upstreams: []*Upstream{
			{Name: "booking", URL: bu, Prefixes: []string{"/api/reservations", "/api/availability"}, Timeout: time.Second},
			{Name: "booking-query", URL: qu, Prefixes: []string{"/api/reservations"}, Methods: []string{http.MethodGet, http.MethodHead}, Timeout: time.Second},
		},
	}
	g.Init()
	id := uuid.NewString()
	cases := []struct{ method, path, want string }{
		{http.MethodGet, "/api/reservations/my", "query GET /api/reservations/my"},
		{http.MethodGet, "/api/reservations/" + id, "query GET /api/reservations/" + id},
		{http.MethodGet, "/api/reservations/summary", "query GET /api/reservations/summary"},
		{http.MethodPost, "/api/reservations", "booking POST /api/reservations"},
		{http.MethodPost, "/api/reservations/" + id + "/cancel", "booking POST /api/reservations/" + id + "/cancel"},
		// Доступность и аудит остаются в booking, даже на GET.
		{http.MethodGet, "/api/availability", "booking GET /api/availability"},
	}
	for _, c := range cases {
		got.Store("")
		if w := do(g, c.method, c.path, ""); w.Code != http.StatusOK {
			t.Fatalf("%s %s: HTTP %d", c.method, c.path, w.Code)
		}
		if got.Load() != c.want {
			t.Errorf("%s %s routed to %q, want %q", c.method, c.path, got.Load(), c.want)
		}
	}
}
