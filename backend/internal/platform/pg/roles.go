package pg

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pools — две роли сервиса (РХД ПР-2, TRADEOFFS T111).
//
//   - Owner владеет базой и таблицами: миграции, ретранслятор outbox,
//     служебные задачи. Только владелец может снять RLS или удалить триггер
//     неизменяемости аудита.
//   - App — роль запросов пользователей: SELECT/INSERT/UPDATE/DELETE на
//     таблицах схемы public и ничего больше, плюс отзыв прав по Deny
//     (например, UPDATE/DELETE на audit_log). Снять RLS она не может.
//
// Если APP_DATABASE_URL не задан, обе роли — одна (старый режим), и сервис
// пишет предупреждение.
type Pools struct {
	Owner, App *pgxpool.Pool
}

func (p Pools) Close() {
	if p.App != nil && p.App != p.Owner {
		p.App.Close()
	}
	if p.Owner != nil {
		p.Owner.Close()
	}
}

// Deny — права, которых роль App не получает на таблице. Grant, если задан,
// выдаётся после отзыва: так табличное право заменяется правом на один
// столбец (например, "UPDATE (row_lock)" — его хватает для SELECT … FOR SHARE).
type Deny struct{ Table, Privs, Grant string }

// OpenPools подключает владельца, применяет миграции, подключает App и
// выдаёт ей права. Права выдаются при каждом старте: это идемпотентно и
// покрывает таблицы из новых миграций.
func OpenPools(ctx context.Context, log *slog.Logger, ownerDSN, appDSN string, appConns int32, migrations fs.FS, deny ...Deny) (Pools, error) {
	ownerConns := int32(4)
	if appDSN == "" {
		ownerConns = appConns
	}
	owner, err := Connect(ctx, log, ownerDSN, ownerConns)
	if err != nil {
		return Pools{}, err
	}
	if migrations != nil {
		if err := Migrate(ctx, owner, migrations, "."); err != nil {
			owner.Close()
			return Pools{}, fmt.Errorf("migrate: %w", err)
		}
	}
	if appDSN == "" {
		log.Warn("APP_DATABASE_URL not set: requests run under the table owner role")
		return Pools{Owner: owner, App: owner}, nil
	}
	app, err := Connect(ctx, log, appDSN, appConns)
	if err != nil {
		owner.Close()
		return Pools{}, err
	}
	var role string
	if err := app.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		app.Close()
		owner.Close()
		return Pools{}, err
	}
	if err := GrantApp(ctx, owner, role, deny...); err != nil {
		app.Close()
		owner.Close()
		return Pools{}, fmt.Errorf("grant %s: %w", role, err)
	}
	return Pools{Owner: owner, App: app}, nil
}

// GrantApp выдаёт роли app права на данные схемы public.
func GrantApp(ctx context.Context, owner *pgxpool.Pool, app string, deny ...Deny) error {
	r := pgx.Identifier{app}.Sanitize()
	return pgx.BeginFunc(ctx, owner, func(tx pgx.Tx) error {
		stmts := []string{
			`GRANT USAGE ON SCHEMA public TO ` + r,
			`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + r,
			`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + r,
			`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ` + r,
			`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO ` + r,
			`REVOKE ALL ON schema_migrations FROM ` + r,
		}
		for _, d := range deny {
			stmts = append(stmts, `REVOKE `+d.Privs+` ON `+pgx.Identifier{d.Table}.Sanitize()+` FROM `+r)
			if d.Grant != "" {
				stmts = append(stmts, `GRANT `+d.Grant+` ON `+pgx.Identifier{d.Table}.Sanitize()+` TO `+r)
			}
		}
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
}
