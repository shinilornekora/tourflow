// Package app — сценарии identity. Хранилище, хэширование и подпись токенов
// приходят снаружи через порты (интерфейсы), main собирает всё вручную.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/identity/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
)

// ---------- порты ----------

type UserFilter struct {
	Role   auth.Role
	Query  string
	Limit  int
	Offset int
}

type Users interface {
	Create(ctx context.Context, u domain.User) error
	ByEmail(ctx context.Context, email domain.Email) (domain.User, error)
	ByID(ctx context.Context, id uuid.UUID) (domain.User, error)
	// Update сохраняет пользователя, если версия в БД совпадает с u.Version;
	// иначе — конфликт (оптимистическая блокировка).
	Update(ctx context.Context, u domain.User) (domain.User, error)
	List(ctx context.Context, f UserFilter) ([]domain.User, int, error)
	CountAdmins(ctx context.Context) (int, error)
}

type Tokens interface {
	Save(ctx context.Context, t domain.RefreshToken) error
	// Rotate атомарно отзывает old и сохраняет next. Возвращает текущую
	// запись old (чтобы распознать повторное использование).
	Rotate(ctx context.Context, oldHash []byte, next domain.RefreshToken, now time.Time) (domain.RefreshToken, error)
	ByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	RevokeFamily(ctx context.Context, family uuid.UUID, now time.Time) error
	RevokeAllForUser(ctx context.Context, user uuid.UUID, now time.Time) error
}

type Hasher interface {
	Hash(password string) (string, error)
	Compare(hash, password string) bool
}

type TokenIssuer interface {
	Issue(userID uuid.UUID, role auth.Role) (string, time.Time, error)
}

// ---------- сервис ----------

type Service struct {
	Users      Users
	Tokens     Tokens
	Hasher     Hasher
	Issuer     TokenIssuer
	RefreshTTL time.Duration
	Now        func() time.Time

	dummyOnce sync.Once
	dummy     string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

type Session struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             domain.User
}

type RegisterInput struct {
	Email    string
	Password string
	FullName string
	Phone    string
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (Session, error) {
	fe := apperr.FieldErrors{}
	email, err := domain.ParseEmail(in.Email)
	if err != nil {
		mergeFields(fe, err)
	}
	if err := domain.ValidatePassword(in.Password); err != nil {
		mergeFields(fe, err)
	}
	profile, err := domain.Profile{FullName: in.FullName, Phone: in.Phone}.Normalize()
	if err != nil {
		mergeFields(fe, err)
	}
	if err := fe.Err(); err != nil {
		return Session{}, err
	}
	hash, err := s.Hasher.Hash(in.Password)
	if err != nil {
		return Session{}, err
	}
	u := domain.NewCustomer(email, hash, profile, s.now())
	if err := s.Users.Create(ctx, u); err != nil {
		return Session{}, err
	}
	return s.newSession(ctx, u, uuid.New())
}

// dummyHash выравнивает время ответа для несуществующего email, чтобы
// по задержке нельзя было перебирать зарегистрированные адреса.
func (s *Service) dummyHash() string {
	s.dummyOnce.Do(func() { s.dummy, _ = s.Hasher.Hash("timing-equalizer-1") })
	return s.dummy
}

func (s *Service) Login(ctx context.Context, rawEmail, password string) (Session, error) {
	bad := apperr.Unauthorized("Неверный email или пароль")
	email, err := domain.ParseEmail(rawEmail)
	if err != nil {
		s.Hasher.Compare(s.dummyHash(), password)
		return Session{}, bad
	}
	u, err := s.Users.ByEmail(ctx, email)
	if apperr.Is(err, apperr.KindNotFound) {
		s.Hasher.Compare(s.dummyHash(), password)
		return Session{}, bad
	}
	if err != nil {
		return Session{}, err
	}
	if !s.Hasher.Compare(u.PasswordHash, password) {
		return Session{}, bad
	}
	return s.newSession(ctx, u, uuid.New())
}

// Refresh выдаёт новую пару и отзывает предъявленный токен (ротация).
// Если предъявлен уже отозванный токен — это признак кражи: отзываем всю
// семью, и легитимному пользователю придётся войти заново.
func (s *Service) Refresh(ctx context.Context, raw string) (Session, error) {
	bad := apperr.Unauthorized("Сессия истекла, войдите заново")
	hash := hashToken(raw)
	now := s.now()
	cur, err := s.Tokens.ByHash(ctx, hash)
	if apperr.Is(err, apperr.KindNotFound) {
		return Session{}, bad
	}
	if err != nil {
		return Session{}, err
	}
	if cur.RevokedAt != nil {
		_ = s.Tokens.RevokeFamily(ctx, cur.FamilyID, now)
		return Session{}, bad
	}
	if now.After(cur.ExpiresAt) {
		return Session{}, bad
	}
	u, err := s.Users.ByID(ctx, cur.UserID)
	if err != nil {
		return Session{}, bad
	}
	plain, next := s.newRefresh(u.ID, cur.FamilyID, now)
	prev, err := s.Tokens.Rotate(ctx, hash, next, now)
	if err != nil {
		return Session{}, err
	}
	if prev.RevokedAt != nil { // гонка: токен отозвали между ByHash и Rotate
		_ = s.Tokens.RevokeFamily(ctx, cur.FamilyID, now)
		return Session{}, bad
	}
	access, exp, err := s.Issuer.Issue(u.ID, u.Role)
	if err != nil {
		return Session{}, err
	}
	return Session{AccessToken: access, AccessExpiresAt: exp, RefreshToken: plain, RefreshExpiresAt: next.ExpiresAt, User: u}, nil
}

func (s *Service) Logout(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	cur, err := s.Tokens.ByHash(ctx, hashToken(raw))
	if err != nil {
		return nil // выход идемпотентен
	}
	return s.Tokens.RevokeFamily(ctx, cur.FamilyID, s.now())
}

func (s *Service) Me(ctx context.Context) (domain.User, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return domain.User{}, err
	}
	return s.Users.ByID(ctx, p.UserID)
}

func (s *Service) UpdateMe(ctx context.Context, fullName, phone string, version int64) (domain.User, error) {
	p, err := auth.Require(ctx)
	if err != nil {
		return domain.User{}, err
	}
	u, err := s.Users.ByID(ctx, p.UserID)
	if err != nil {
		return domain.User{}, err
	}
	profile, err := domain.Profile{FullName: fullName, Phone: phone}.Normalize()
	if err != nil {
		return domain.User{}, err
	}
	if version != 0 {
		u.Version = version
	}
	u.Profile = profile
	u.UpdatedAt = s.now()
	return s.Users.Update(ctx, u)
}

func (s *Service) ChangePassword(ctx context.Context, current, next string) error {
	p, err := auth.Require(ctx)
	if err != nil {
		return err
	}
	u, err := s.Users.ByID(ctx, p.UserID)
	if err != nil {
		return err
	}
	if !s.Hasher.Compare(u.PasswordHash, current) {
		return apperr.Validation(map[string]string{"current_password": "Текущий пароль указан неверно"})
	}
	if err := domain.ValidatePassword(next); err != nil {
		return err
	}
	hash, err := s.Hasher.Hash(next)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	u.UpdatedAt = s.now()
	if _, err := s.Users.Update(ctx, u); err != nil {
		return err
	}
	// Смена пароля завершает все сессии на других устройствах.
	return s.Tokens.RevokeAllForUser(ctx, u.ID, s.now())
}

func (s *Service) ListUsers(ctx context.Context, f UserFilter) ([]domain.User, int, error) {
	if _, err := auth.RequireRole(ctx, auth.RoleAdmin); err != nil {
		return nil, 0, err
	}
	if f.Role != "" && !f.Role.Valid() {
		return nil, 0, apperr.Validation(map[string]string{"role": "Неизвестная роль"})
	}
	return s.Users.List(ctx, f)
}

func (s *Service) ChangeRole(ctx context.Context, id uuid.UUID, role auth.Role) (domain.User, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleAdmin)
	if err != nil {
		return domain.User{}, err
	}
	u, err := s.Users.ByID(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	if err := u.ChangeRole(actor, role, s.now()); err != nil {
		return domain.User{}, err
	}
	saved, err := s.Users.Update(ctx, u)
	if err != nil {
		return domain.User{}, err
	}
	// Роль зашита в access-токен, поэтому старые refresh-сессии отзываем:
	// новый токен с новой ролью пользователь получит при следующем входе.
	// Уже выданный access-токен проживёт до истечения TTL — это осознанная цена
	// отказа от проверки токена в identity на каждый запрос.
	_ = s.Tokens.RevokeAllForUser(ctx, id, s.now())
	return saved, nil
}

// UserByID — внутренний вызов (catalog проверяет кандидата в менеджеры).
func (s *Service) UserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return s.Users.ByID(ctx, id)
}

// EnsureAdmin создаёт первого администратора, если в системе нет ни одного.
func (s *Service) EnsureAdmin(ctx context.Context, email, password, name string) (bool, error) {
	n, err := s.Users.CountAdmins(ctx)
	if err != nil || n > 0 {
		return false, err
	}
	e, err := domain.ParseEmail(email)
	if err != nil {
		return false, err
	}
	if err := domain.ValidatePassword(password); err != nil {
		return false, err
	}
	hash, err := s.Hasher.Hash(password)
	if err != nil {
		return false, err
	}
	u := domain.NewCustomer(e, hash, domain.Profile{FullName: name}, s.now())
	u.Role = auth.RoleAdmin
	if err := s.Users.Create(ctx, u); err != nil {
		if apperr.Is(err, apperr.KindConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ---------- внутреннее ----------

func (s *Service) newSession(ctx context.Context, u domain.User, family uuid.UUID) (Session, error) {
	access, exp, err := s.Issuer.Issue(u.ID, u.Role)
	if err != nil {
		return Session{}, err
	}
	plain, rt := s.newRefresh(u.ID, family, s.now())
	if err := s.Tokens.Save(ctx, rt); err != nil {
		return Session{}, err
	}
	return Session{AccessToken: access, AccessExpiresAt: exp, RefreshToken: plain, RefreshExpiresAt: rt.ExpiresAt, User: u}, nil
}

func (s *Service) newRefresh(user, family uuid.UUID, now time.Time) (string, domain.RefreshToken) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	plain := base64.RawURLEncoding.EncodeToString(buf)
	ttl := s.RefreshTTL
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return plain, domain.RefreshToken{Hash: hashToken(plain), UserID: user, FamilyID: family, ExpiresAt: now.Add(ttl)}
}

func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func mergeFields(fe apperr.FieldErrors, err error) {
	if e := apperr.As(err); e.Kind == apperr.KindValidation {
		for k, v := range e.Fields {
			fe.Add(k, v)
		}
	}
}
