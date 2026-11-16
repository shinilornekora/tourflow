// Package auth — выпуск и проверка access-токенов.
//
// Токен подписывается Ed25519 (EdDSA) приватным ключом identity. Остальные
// сервисы и gateway знают только публичный ключ, который забирают из
// /.well-known/jwks.json. Общего секрета (как при HS256) нет: компрометация
// любого сервиса кроме identity не даёт выпускать токены.
//
// В токене только sub и role. Принадлежность менеджера к отелю в токен
// намеренно не кладётся: она меняется чаще, чем живёт токен, поэтому каждый
// сервис проверяет её по своим данным.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
)

type Role string

const (
	RoleCustomer Role = "customer"
	RoleManager  Role = "hotel_manager"
	RoleAdmin    Role = "admin"
	// RoleSystem — внутренний актор фоновых процессов (обработчики событий).
	// В токенах не встречается.
	RoleSystem Role = "system"
)

func (r Role) Valid() bool {
	switch r {
	case RoleCustomer, RoleManager, RoleAdmin:
		return true
	}
	return false
}

const (
	Issuer   = "tourflow-identity"
	Audience = "tourflow"
)

// Principal — аутентифицированный пользователь запроса.
type Principal struct {
	UserID uuid.UUID
	Role   Role
}

func (p Principal) Is(roles ...Role) bool {
	for _, r := range roles {
		if p.Role == r {
			return true
		}
	}
	return false
}

type claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// ---------- выпуск (только identity) ----------

type Signer struct {
	key ed25519.PrivateKey
	kid string
	ttl time.Duration
	now func() time.Time
}

// NewSigner принимает seed ключа в base64 (32 байта). Если seed пустой,
// генерируется случайный ключ — удобно в dev, но токены не переживут рестарт.
func NewSigner(seedB64 string, ttl time.Duration) (*Signer, error) {
	var priv ed25519.PrivateKey
	if seedB64 == "" {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		priv = k
	} else {
		seed, err := base64.StdEncoding.DecodeString(seedB64)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("JWT_SIGNING_SEED must be base64 of %d bytes", ed25519.SeedSize)
		}
		priv = ed25519.NewKeyFromSeed(seed)
	}
	pub := priv.Public().(ed25519.PublicKey)
	kid := base64.RawURLEncoding.EncodeToString(pub[:8])
	return &Signer{key: priv, kid: kid, ttl: ttl, now: time.Now}, nil
}

func (s *Signer) Issue(userID uuid.UUID, role Role) (string, time.Time, error) {
	now := s.now()
	exp := now.Add(s.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
	})
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	return signed, exp, err
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

// JWKSHandler отдаёт публичный ключ.
func (s *Signer) JWKSHandler() http.HandlerFunc {
	pub := s.key.Public().(ed25519.PublicKey)
	body, _ := json.Marshal(jwks{Keys: []jwk{{
		Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub),
		Kid: s.kid, Use: "sig", Alg: "EdDSA",
	}}})
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
	}
}

// PublicKeys — для проверки внутри самого identity без сети.
func (s *Signer) PublicKeys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{s.kid: s.key.Public().(ed25519.PublicKey)}
}

// ---------- проверка (все сервисы) ----------

// KeySource отдаёт публичные ключи по kid.
type KeySource interface {
	Keys(ctx context.Context) (map[string]ed25519.PublicKey, error)
}

// StaticKeys — источник ключей в памяти.
type StaticKeys map[string]ed25519.PublicKey

func (s StaticKeys) Keys(context.Context) (map[string]ed25519.PublicKey, error) { return s, nil }

// RemoteJWKS скачивает ключи у identity и кэширует их на TTL. Если identity
// недоступен, используется последний удачный набор: временная недоступность
// identity не должна ронять проверку токенов во всей системе.
type RemoteJWKS struct {
	url    string
	ttl    time.Duration
	client *http.Client

	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey
	fetched time.Time
}

func NewRemoteJWKS(url string, ttl time.Duration) *RemoteJWKS {
	return &RemoteJWKS{url: url, ttl: ttl, client: &http.Client{Timeout: 2 * time.Second}}
}

func (r *RemoteJWKS) Keys(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys != nil && time.Since(r.fetched) < r.ttl {
		return r.keys, nil
	}
	keys, err := r.fetch(ctx)
	if err != nil {
		if r.keys != nil {
			return r.keys, nil // работаем на устаревшем наборе
		}
		return nil, err
	}
	r.keys, r.fetched = keys, time.Now()
	return keys, nil
}

// Invalidate сбрасывает кэш (например, при неизвестном kid — ротация ключа).
func (r *RemoteJWKS) Invalidate() {
	r.mu.Lock()
	r.fetched = time.Time{}
	r.mu.Unlock()
}

func (r *RemoteJWKS) fetch(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}
	var set jwks
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	out := make(map[string]ed25519.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		out[k.Kid] = ed25519.PublicKey(raw)
	}
	if len(out) == 0 {
		return nil, errors.New("jwks has no usable keys")
	}
	return out, nil
}

type Verifier struct {
	src KeySource
}

func NewVerifier(src KeySource) *Verifier { return &Verifier{src: src} }

var errBadToken = apperr.Unauthorized("Сессия недействительна, войдите заново")

func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	keys, err := v.src.Keys(ctx)
	if err != nil {
		return Principal{}, apperr.Unavailable("auth_keys_unavailable", "Сервис авторизации недоступен", err)
	}
	var c claims
	_, err = jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, ok := keys[kid]
		if !ok {
			if inv, ok := v.src.(interface{ Invalidate() }); ok {
				inv.Invalidate()
			}
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return k, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		return Principal{}, errBadToken
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil || !c.Role.Valid() {
		return Principal{}, errBadToken
	}
	return Principal{UserID: id, Role: c.Role}, nil
}

// ---------- контекст и middleware ----------

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext возвращает пользователя запроса, если он аутентифицирован.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Require возвращает пользователя или ошибку unauthorized.
func Require(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, apperr.Unauthorized("Требуется вход в систему")
	}
	return p, nil
}

// RequireRole проверяет роль пользователя.
func RequireRole(ctx context.Context, roles ...Role) (Principal, error) {
	p, err := Require(ctx)
	if err != nil {
		return p, err
	}
	if !p.Is(roles...) {
		return p, apperr.Forbidden("Недостаточно прав для этого действия")
	}
	return p, nil
}

// BearerToken достаёт токен из заголовка Authorization.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Middleware проверяет токен, если он есть. Запрос без токена проходит
// анонимно — решение «нужен ли вход» принимает конкретный обработчик.
// Неверный токен отклоняется сразу.
func (v *Verifier) Middleware(onError func(http.ResponseWriter, *http.Request, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := BearerToken(r)
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}
			p, err := v.Verify(r.Context(), raw)
			if err != nil {
				onError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}
