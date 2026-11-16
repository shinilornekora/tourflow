// Package directory — синхронный клиент к identity с бюджетом времени и
// circuit breaker (решения из ПКП-4).
package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/logx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/resilience"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Breaker *resilience.Breaker
	// Budget — сколько максимум ждём identity. Админ ждёт ответа на экране,
	// поэтому бюджет короткий: лучше честная ошибка, чем зависшая кнопка.
	Budget time.Duration
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL, Token: token,
		HTTP:    &http.Client{Timeout: 3 * time.Second},
		Breaker: resilience.NewBreaker(5, 10*time.Second),
		Budget:  800 * time.Millisecond,
	}
}

var errUnavailable = apperr.Unavailable("identity_unavailable", "Сервис пользователей временно недоступен, повторите позже", nil)

func (c *Client) Role(ctx context.Context, id uuid.UUID) (auth.Role, error) {
	if err := c.Breaker.Allow(); err != nil {
		return "", errUnavailable
	}
	role, err := c.do(ctx, id)
	// Ответ 404 — нормальный ответ, не отказ зависимости.
	c.Breaker.Done(err == nil || apperr.Is(err, apperr.KindNotFound))
	return role, err
}

func (c *Client) do(ctx context.Context, id uuid.UUID) (auth.Role, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Budget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/internal/users/%s", c.BaseURL, id), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Token", c.Token)
	req.Header.Set(httpx.RequestIDHeader, logx.RequestID(ctx))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", apperr.Unavailable("identity_timeout", "Сервис пользователей не ответил вовремя", err)
		}
		return "", apperr.Unavailable("identity_unavailable", "Сервис пользователей недоступен", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", apperr.NotFound("Пользователь")
	default:
		return "", apperr.Unavailable("identity_error", "Сервис пользователей ответил ошибкой", fmt.Errorf("status %d", resp.StatusCode))
	}
	var body struct {
		Role auth.Role `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", apperr.Unavailable("identity_bad_response", "Некорректный ответ сервиса пользователей", err)
	}
	return body.Role, nil
}
