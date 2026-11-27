package migrations

import (
	"embed"

	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

//go:embed *.sql
var FS embed.FS

// Deny — роль данных (API чтения) не пишет ни в одну таблицу: проекцию
// ведёт только проектор под владельцем. «Проектор пишет · API читает» —
// по правам, а не по соглашению. Inbox роли данных не виден вовсе.
var Deny = []pg.Deny{
	{Table: "reservations_cards", Privs: "INSERT, UPDATE, DELETE"},
	{Table: "hotel_managers", Privs: "INSERT, UPDATE, DELETE"},
	{Table: "hotel_versions", Privs: "INSERT, UPDATE, DELETE"},
	{Table: "projection_inbox", Privs: "ALL"},
}
