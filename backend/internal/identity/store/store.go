// Package store — адаптер PostgreSQL для identity.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/shinilornekora/tourflow/backend/internal/identity/app"
	"github.com/shinilornekora/tourflow/backend/internal/identity/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

type Users struct{ Pool *pgxpool.Pool }

const userCols = `id, email, password_hash, full_name, phone, role, created_at, updated_at, version`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	var email, role string
	err := row.Scan(&u.ID, &email, &u.PasswordHash, &u.Profile.FullName, &u.Profile.Phone, &role, &u.CreatedAt, &u.UpdatedAt, &u.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, apperr.NotFound("Пользователь")
	}
	if err != nil {
		return u, err
	}
	u.Email, _ = domain.ParseEmail(email)
	u.Role = auth.Role(role)
	return u, nil
}

func (s Users) Create(ctx context.Context, u domain.User) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO users(`+userCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		u.ID, u.Email.String(), u.PasswordHash, u.Profile.FullName, u.Profile.Phone, string(u.Role), u.CreatedAt, u.UpdatedAt, u.Version)
	if pg.IsUniqueViolation(err, "users_email_key") {
		return apperr.Conflict("email_taken", "Пользователь с таким email уже зарегистрирован")
	}
	return err
}

func (s Users) ByEmail(ctx context.Context, e domain.Email) (domain.User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE email = $1`, e.String()))
}

func (s Users) ByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

func (s Users) Update(ctx context.Context, u domain.User) (domain.User, error) {
	row := s.Pool.QueryRow(ctx, `UPDATE users SET password_hash=$3, full_name=$4, phone=$5, role=$6,
		updated_at=$7, version = version + 1
		WHERE id=$1 AND version=$2 RETURNING `+userCols,
		u.ID, u.Version, u.PasswordHash, u.Profile.FullName, u.Profile.Phone, string(u.Role), u.UpdatedAt)
	saved, err := scanUser(row)
	if apperr.Is(err, apperr.KindNotFound) {
		var exists bool
		_ = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, u.ID).Scan(&exists)
		if exists {
			return saved, apperr.Conflict("version_mismatch", "Данные изменились в другом окне, обновите страницу")
		}
	}
	return saved, err
}

func (s Users) List(ctx context.Context, f app.UserFilter) ([]domain.User, int, error) {
	where := `WHERE ($1 = '' OR role = $1) AND ($2 = '' OR email ILIKE '%' || $2 || '%' OR full_name ILIKE '%' || $2 || '%')`
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM users `+where, string(f.Role), f.Query).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+userCols+` FROM users `+where+` ORDER BY created_at DESC, id LIMIT $3 OFFSET $4`,
		string(f.Role), f.Query, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

func (s Users) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&n)
	return n, err
}

// ---------- refresh tokens ----------

type Tokens struct{ Pool *pgxpool.Pool }

func (s Tokens) Save(ctx context.Context, t domain.RefreshToken) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO refresh_tokens(token_hash, user_id, family_id, expires_at) VALUES ($1,$2,$3,$4)`,
		t.Hash, t.UserID, t.FamilyID, t.ExpiresAt)
	return err
}

func scanToken(row pgx.Row) (domain.RefreshToken, error) {
	var t domain.RefreshToken
	err := row.Scan(&t.Hash, &t.UserID, &t.FamilyID, &t.ExpiresAt, &t.RevokedAt, &t.ReplacedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, apperr.NotFound("Сессия")
	}
	return t, err
}

func (s Tokens) ByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	return scanToken(s.Pool.QueryRow(ctx, `SELECT token_hash, user_id, family_id, expires_at, revoked_at, replaced_by
		FROM refresh_tokens WHERE token_hash = $1`, hash))
}

// Rotate блокирует старую запись (FOR UPDATE), поэтому два параллельных
// refresh одним токеном не получат две валидные пары: второй увидит
// revoked_at и вызовет отзыв семьи.
func (s Tokens) Rotate(ctx context.Context, oldHash []byte, next domain.RefreshToken, now time.Time) (domain.RefreshToken, error) {
	var prev domain.RefreshToken
	err := pg.InTx(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		prev, err = scanToken(tx.QueryRow(ctx, `SELECT token_hash, user_id, family_id, expires_at, revoked_at, replaced_by
			FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, oldHash))
		if err != nil || prev.RevokedAt != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=$2, replaced_by=$3 WHERE token_hash=$1`, oldHash, now, next.Hash); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO refresh_tokens(token_hash, user_id, family_id, expires_at) VALUES ($1,$2,$3,$4)`,
			next.Hash, next.UserID, next.FamilyID, next.ExpiresAt)
		return err
	})
	return prev, err
}

func (s Tokens) RevokeFamily(ctx context.Context, family uuid.UUID, now time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE family_id = $1 AND revoked_at IS NULL`, family, now)
	return err
}

func (s Tokens) RevokeAllForUser(ctx context.Context, user uuid.UUID, now time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, user, now)
	return err
}

// ---------- bcrypt ----------

// Bcrypt — хэширование паролей. Cost настраивается: 12 в prod, 4 в тестах.
type Bcrypt struct{ Cost int }

func (b Bcrypt) Hash(p string) (string, error) {
	cost := b.Cost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), cost)
	return string(h), err
}

func (b Bcrypt) Compare(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) == nil
}
