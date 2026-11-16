package migrations

import (
	"embed"

	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

//go:embed *.sql
var FS embed.FS

// Deny — чего роль данных (запросы пользователей) не получает:
//   - outbox: только INSERT; читает и отмечает ретранслятор под владельцем;
//   - audit_log: только SELECT и INSERT — изменить или удалить запись
//     аудита роль запросов не может по правам, а не только из-за триггера;
//   - reconciliation_issues: пишет только служебная сверка;
//   - реплика каталога и inbox: пишет только проекция событий под владельцем.
//     На hotel_manager_replica держится политика RLS (app_can_see_hotel):
//     роль запросов не должна уметь назначить себя менеджером отеля.
//     Бронирование блокирует строки реплики SELECT … FOR SHARE, а для этого
//     PostgreSQL требует UPDATE хотя бы на один столбец. Роль данных получает
//     UPDATE только на служебный столбец row_lock (004): цену, вместимость
//     и статус в реплике она изменить не может;
//   - reservations, inventory, idempotency_keys: без DELETE — сценарии
//     пользователей строки не удаляют, ключи чистит служебная задача.
var Deny = []pg.Deny{
	{Table: "hotel_manager_replica", Privs: "INSERT, UPDATE, DELETE"},
	{Table: "hotel_replica", Privs: "INSERT, UPDATE, DELETE", Grant: "UPDATE (row_lock)"},
	{Table: "room_type_replica", Privs: "INSERT, UPDATE, DELETE", Grant: "UPDATE (row_lock)"},
	{Table: "processed_events", Privs: "ALL"},
	{Table: "reservations", Privs: "DELETE"},
	{Table: "inventory", Privs: "DELETE"},
	{Table: "idempotency_keys", Privs: "DELETE"},
	{Table: "outbox", Privs: "SELECT, UPDATE, DELETE"},
	{Table: "audit_log", Privs: "UPDATE, DELETE"},
	{Table: "reconciliation_issues", Privs: "ALL"},
}
