// Package domain — модель пользователя и её инварианты.
package domain

import (
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
)

// Email — value object: нормализован (trim + lower) и проверен при создании.
type Email struct{ v string }

func ParseEmail(raw string) (Email, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) > 254 {
		return Email{}, apperr.Validation(map[string]string{"email": "Слишком длинный email"})
	}
	addr, err := mail.ParseAddress(v)
	if err != nil || addr.Address != v || !strings.Contains(v[strings.LastIndex(v, "@")+1:], ".") {
		return Email{}, apperr.Validation(map[string]string{"email": "Некорректный email"})
	}
	return Email{v: v}, nil
}

func (e Email) String() string { return e.v }

// ValidatePassword — политика паролей: 8..72 байта (предел bcrypt),
// хотя бы одна буква и одна цифра.
func ValidatePassword(p string) error {
	if len(p) < 8 {
		return apperr.Validation(map[string]string{"password": "Минимум 8 символов"})
	}
	if len(p) > 72 {
		return apperr.Validation(map[string]string{"password": "Не длиннее 72 байт"})
	}
	var letter, digit bool
	for _, r := range p {
		switch {
		case unicode.IsLetter(r):
			letter = true
		case unicode.IsDigit(r):
			digit = true
		}
	}
	if !letter || !digit {
		return apperr.Validation(map[string]string{"password": "Нужны хотя бы одна буква и одна цифра"})
	}
	return nil
}

// Profile — редактируемые пользователем поля.
type Profile struct {
	FullName string
	Phone    string
}

func (p Profile) Normalize() (Profile, error) {
	fe := apperr.FieldErrors{}
	p.FullName = strings.Join(strings.Fields(p.FullName), " ")
	p.Phone = strings.TrimSpace(p.Phone)
	if n := utf8.RuneCountInString(p.FullName); n < 2 || n > 200 {
		fe.Add("full_name", "Укажите имя (2–200 символов)")
	}
	if p.Phone != "" {
		digits := 0
		for _, r := range p.Phone {
			switch {
			case unicode.IsDigit(r):
				digits++
			case strings.ContainsRune("+-() ", r):
			default:
				fe.Add("phone", "Телефон может содержать только цифры, пробелы, +, -, ()")
			}
		}
		if digits < 10 || digits > 15 {
			fe.Add("phone", "В телефоне должно быть 10–15 цифр")
		}
	}
	return p, fe.Err()
}

type User struct {
	ID           uuid.UUID
	Email        Email
	PasswordHash string
	Profile      Profile
	Role         auth.Role
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Version      int64
}

// NewCustomer — самостоятельная регистрация всегда создаёт клиента.
// Менеджера отеля и администратора назначает только администратор.
func NewCustomer(email Email, hash string, profile Profile, now time.Time) User {
	return User{
		ID: uuid.New(), Email: email, PasswordHash: hash, Profile: profile,
		Role: auth.RoleCustomer, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
}

// ChangeRole — инвариант: администратор не может снять роль сам с себя,
// иначе система может остаться без администратора.
func (u *User) ChangeRole(actor auth.Principal, role auth.Role, now time.Time) error {
	if !role.Valid() {
		return apperr.Validation(map[string]string{"role": "Неизвестная роль"})
	}
	if actor.UserID == u.ID && role != auth.RoleAdmin {
		return apperr.Conflict("self_demotion", "Нельзя снять роль администратора с самого себя")
	}
	u.Role = role
	u.UpdatedAt = now
	return nil
}

// RefreshToken — запись о refresh-токене (в БД хранится хэш).
type RefreshToken struct {
	Hash       []byte
	UserID     uuid.UUID
	FamilyID   uuid.UUID
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy []byte
}
