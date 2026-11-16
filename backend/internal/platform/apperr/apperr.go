// Package apperr — единый словарь ошибок прикладного уровня.
// Домен и use case возвращают *Error с Kind, HTTP-адаптер переводит Kind
// в статус. Так домен ничего не знает про HTTP (гексагональная граница, ПКП-2).
package apperr

import (
	"errors"
	"fmt"
)

type Kind string

const (
	KindValidation   Kind = "validation"
	KindNotFound     Kind = "not_found"
	KindConflict     Kind = "conflict"
	KindForbidden    Kind = "forbidden"
	KindUnauthorized Kind = "unauthorized"
	KindUnavailable  Kind = "unavailable"
	KindInternal     Kind = "internal"
)

type Error struct {
	Kind    Kind
	Code    string // машинный код: "no_availability", "version_mismatch"...
	Message string // человеко-читаемое сообщение (русский, уходит в UI)
	Fields  map[string]string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, code, msg string) *Error {
	return &Error{Kind: kind, Code: code, Message: msg}
}

func Validation(fields map[string]string) *Error {
	return &Error{Kind: KindValidation, Code: "validation_failed", Message: "Проверьте введённые данные", Fields: fields}
}

func NotFound(what string) *Error {
	return &Error{Kind: KindNotFound, Code: "not_found", Message: what + " не найден(а)"}
}

func Conflict(code, msg string) *Error { return New(KindConflict, code, msg) }

func Forbidden(msg string) *Error { return New(KindForbidden, "forbidden", msg) }

func Unauthorized(msg string) *Error { return New(KindUnauthorized, "unauthorized", msg) }

func Unavailable(code, msg string, err error) *Error {
	return &Error{Kind: KindUnavailable, Code: code, Message: msg, Err: err}
}

// As достаёт *Error из цепочки; для чужих ошибок возвращает internal.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: KindInternal, Code: "internal", Message: "Внутренняя ошибка сервиса", Err: err}
}

// Is проверяет вид ошибки.
func Is(err error, kind Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// FieldErrors накапливает ошибки валидации по полям.
type FieldErrors map[string]string

func (f FieldErrors) Add(field, msg string) {
	if _, exists := f[field]; !exists {
		f[field] = msg
	}
}

// Err возвращает nil, если ошибок нет.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return Validation(f)
}
