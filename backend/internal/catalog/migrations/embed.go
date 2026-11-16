package migrations

import (
	"embed"

	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

//go:embed *.sql
var FS embed.FS

// Deny — роль данных только добавляет события в outbox; читает и отмечает
// их ретранслятор под ролью владельца.
var Deny = []pg.Deny{{Table: "outbox", Privs: "SELECT, UPDATE, DELETE"}}
