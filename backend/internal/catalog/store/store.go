// Package store — PostgreSQL-адаптер каталога.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/catalog/app"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

type Store struct{ Pool *pgxpool.Pool }

var _ app.Store = Store{}

// write — транзакция изменения с контекстом RLS: кто действует, берётся из
// проверенного токена в ctx. Без личности роль пустая, и политики каталога
// не пропустят ни одной записи (fail closed).
func (s Store) write(ctx context.Context, fn func(pgx.Tx) error) error {
	return pg.InTx(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		user, role := "", ""
		if p, ok := auth.FromContext(ctx); ok {
			user, role = p.UserID.String(), string(p.Role)
		}
		if err := pg.SetActor(ctx, tx, user, role); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (s Store) CreateHotel(ctx context.Context, h domain.Hotel, ev messaging.Envelope) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO hotels(id, name, city, address, description, stars, photo_url, status, created_at, updated_at, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			h.ID, h.Name, h.City, h.Address, h.Description, h.Stars, h.PhotoURL, string(h.Status), h.CreatedAt, h.UpdatedAt, h.Version); err != nil {
			return err
		}
		return messaging.WriteOutbox(ctx, tx, messaging.TopicHotels, ev)
	})
}

var errStale = apperr.Conflict("version_mismatch", "Данные изменились параллельно, повторите действие")

func (s Store) SaveHotel(ctx context.Context, h domain.Hotel, expected int64, ev messaging.Envelope) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE hotels SET name=$3, city=$4, address=$5, description=$6, stars=$7, photo_url=$8,
			status=$9, updated_at=$10, version=$11 WHERE id=$1 AND version=$2`,
			h.ID, expected, h.Name, h.City, h.Address, h.Description, h.Stars, h.PhotoURL, string(h.Status), h.UpdatedAt, h.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errStale
		}
		// Список менеджеров — часть агрегата «отель»: заменяем целиком в той же
		// транзакции, что и версию, поэтому событие всегда несёт согласованный набор.
		if _, err := tx.Exec(ctx, `DELETE FROM hotel_managers WHERE hotel_id=$1 AND NOT (user_id = ANY($2))`, h.ID, h.ManagerIDs); err != nil {
			return err
		}
		// Вставляются только недостающие назначения. Когда отель правит
		// менеджер, список не меняется и вставлять нечего: политика RLS
		// разрешает менять hotel_managers только администратору (003).
		if _, err := tx.Exec(ctx, `INSERT INTO hotel_managers(hotel_id, user_id)
			SELECT $1, u FROM unnest($2::uuid[]) AS u
			WHERE NOT EXISTS (SELECT 1 FROM hotel_managers m WHERE m.hotel_id = $1 AND m.user_id = u)`, h.ID, h.ManagerIDs); err != nil {
			return err
		}
		return messaging.WriteOutbox(ctx, tx, messaging.TopicHotels, ev)
	})
}

const hotelCols = `h.id, h.name, h.city, h.address, h.description, h.stars, h.photo_url, h.status, h.created_at, h.updated_at, h.version,
	COALESCE((SELECT array_agg(m.user_id ORDER BY m.assigned_at) FROM hotel_managers m WHERE m.hotel_id = h.id), '{}')`

func scanHotel(row pgx.Row, extra ...any) (domain.Hotel, error) {
	var h domain.Hotel
	var status string
	dest := append([]any{&h.ID, &h.Name, &h.City, &h.Address, &h.Description, &h.Stars, &h.PhotoURL, &status, &h.CreatedAt, &h.UpdatedAt, &h.Version, &h.ManagerIDs}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return h, apperr.NotFound("Отель")
		}
		return h, err
	}
	h.Status = domain.HotelStatus(status)
	return h, nil
}

func (s Store) Hotel(ctx context.Context, id uuid.UUID) (domain.Hotel, error) {
	return scanHotel(s.Pool.QueryRow(ctx, `SELECT `+hotelCols+` FROM hotels h WHERE h.id = $1`, id))
}

// ListHotels строит запрос из фиксированного набора условий с параметрами —
// никакой конкатенации пользовательского ввода. Сортировка — из белого списка.
//
// Пагинация — OFFSET/LIMIT: интерфейсу нужны номера страниц и общее число.
// На глубоких страницах OFFSET дорогой, но каталог отелей турфирмы — это
// тысячи строк, а не миллионы; keyset-пагинация оставлена для журналов броней.
func (s Store) ListHotels(ctx context.Context, f app.HotelFilter) ([]app.HotelSummary, int, error) {
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if !f.IncludeInactive {
		conds = append(conds, "h.status = 'active'")
	}
	if f.ManagedBy != uuid.Nil {
		conds = append(conds, "EXISTS (SELECT 1 FROM hotel_managers m WHERE m.hotel_id = h.id AND m.user_id = "+arg(f.ManagedBy)+")")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		conds = append(conds, "h.name ILIKE '%' || "+arg(escapeLike(q))+" || '%'")
	}
	if c := strings.TrimSpace(f.City); c != "" {
		conds = append(conds, "lower(h.city) = lower("+arg(c)+")")
	}
	if f.StarsMin > 0 {
		conds = append(conds, "h.stars >= "+arg(f.StarsMin))
	}
	if f.Guests > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM room_types r WHERE r.hotel_id = h.id AND r.status='active' AND r.capacity >= "+arg(f.Guests)+")")
	}
	if f.PriceMax > 0 {
		conds = append(conds, "p.min_price <= "+arg(f.PriceMax))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	from := `FROM hotels h
		LEFT JOIN LATERAL (SELECT min(r.price_amount) AS min_price, count(*) AS cnt
			FROM room_types r WHERE r.hotel_id = h.id AND r.status = 'active') p ON true `

	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) `+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := map[string]string{
		"": "h.name, h.id", "name": "h.name, h.id", "-stars": "h.stars DESC, h.name, h.id",
		"price": "p.min_price ASC NULLS LAST, h.id", "-price": "p.min_price DESC NULLS LAST, h.id",
		"-created_at": "h.created_at DESC, h.id",
	}[f.Sort]
	limit, offset := arg(f.Limit), arg(f.Offset)
	rows, err := s.Pool.Query(ctx, `SELECT `+hotelCols+`, p.min_price, p.cnt `+from+where+` ORDER BY `+order+` LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []app.HotelSummary{}
	for rows.Next() {
		var minPrice *int64
		var cnt int
		h, err := scanHotel(rows, &minPrice, &cnt)
		if err != nil {
			return nil, 0, err
		}
		sum := app.HotelSummary{Hotel: h, RoomTypeCount: cnt}
		if minPrice != nil {
			sum.MinPrice = &domain.Money{Amount: *minPrice, Currency: "RUB"}
		}
		out = append(out, sum)
	}
	return out, total, rows.Err()
}

func (s Store) Cities(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (lower(city)) city FROM hotels WHERE status='active' ORDER BY lower(city), city`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---------- категории ----------

func (s Store) CreateRoomType(ctx context.Context, r domain.RoomType, ev messaging.Envelope) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO room_types(id, hotel_id, name, description, capacity, total_rooms, price_amount, price_currency,
			amenities, photo_url, status, created_at, updated_at, version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			r.ID, r.HotelID, r.Name, r.Description, r.Capacity, r.TotalRooms, r.Price.Amount, r.Price.Currency,
			r.Amenities, r.PhotoURL, string(r.Status), r.CreatedAt, r.UpdatedAt, r.Version)
		if pg.IsUniqueViolation(err, "room_types_name_per_hotel") {
			return apperr.Conflict("duplicate_name", "В отеле уже есть категория с таким названием")
		}
		if err != nil {
			return err
		}
		return messaging.WriteOutbox(ctx, tx, messaging.TopicRoomTypes, ev)
	})
}

func (s Store) SaveRoomType(ctx context.Context, r domain.RoomType, expected int64, ev messaging.Envelope) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE room_types SET name=$3, description=$4, capacity=$5, total_rooms=$6, price_amount=$7,
			price_currency=$8, amenities=$9, photo_url=$10, status=$11, updated_at=$12, version=$13 WHERE id=$1 AND version=$2`,
			r.ID, expected, r.Name, r.Description, r.Capacity, r.TotalRooms, r.Price.Amount, r.Price.Currency,
			r.Amenities, r.PhotoURL, string(r.Status), r.UpdatedAt, r.Version)
		if pg.IsUniqueViolation(err, "room_types_name_per_hotel") {
			return apperr.Conflict("duplicate_name", "В отеле уже есть категория с таким названием")
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errStale
		}
		return messaging.WriteOutbox(ctx, tx, messaging.TopicRoomTypes, ev)
	})
}

const roomCols = `id, hotel_id, name, description, capacity, total_rooms, price_amount, price_currency, amenities, photo_url, status, created_at, updated_at, version`

func scanRoom(row pgx.Row) (domain.RoomType, error) {
	var r domain.RoomType
	var status, cur string
	err := row.Scan(&r.ID, &r.HotelID, &r.Name, &r.Description, &r.Capacity, &r.TotalRooms, &r.Price.Amount, &cur,
		&r.Amenities, &r.PhotoURL, &status, &r.CreatedAt, &r.UpdatedAt, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, apperr.NotFound("Категория номеров")
	}
	r.Price.Currency = strings.TrimSpace(cur)
	r.Status = domain.RoomStatus(status)
	return r, err
}

func (s Store) RoomType(ctx context.Context, id uuid.UUID) (domain.RoomType, error) {
	return scanRoom(s.Pool.QueryRow(ctx, `SELECT `+roomCols+` FROM room_types WHERE id=$1`, id))
}

func (s Store) RoomTypes(ctx context.Context, hotelID uuid.UUID, includeArchived bool) ([]domain.RoomType, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+roomCols+` FROM room_types WHERE hotel_id=$1 AND ($2 OR status='active')
		ORDER BY price_amount, name`, hotelID, includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RoomType{}
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
