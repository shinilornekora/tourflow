package backend_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServiceBoundaries — архитектурный тест. Сервисы живут в одном
// репозитории и одном Go-модуле, поэтому компилятор не помешает booking
// импортировать catalog/domain. Этот тест мешает: сервис может зависеть
// только от себя и от internal/platform. Общий код — только инфраструктура,
// доменная модель у каждого сервиса своя.
func TestServiceBoundaries(t *testing.T) {
	const mod = "github.com/shinilornekora/tourflow/backend/internal/"
	services := []string{"identity", "catalog", "booking", "bookingquery", "notification", "gateway"}
	err := filepath.WalkDir("internal", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		parts := strings.Split(filepath.ToSlash(path), "/")
		owner := parts[1]
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, mod) {
				continue
			}
			target := strings.Split(strings.TrimPrefix(p, mod), "/")[0]
			if target == owner || target == "platform" {
				continue
			}
			for _, s := range services {
				if target == s {
					t.Errorf("%s imports %s: service %q must not depend on service %q", path, p, owner, target)
				}
			}
		}
		if owner == "platform" {
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				for _, s := range services {
					if strings.HasPrefix(p, mod+s) {
						t.Errorf("%s: platform must not depend on service %s", path, s)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestServicesUseTwoRoles — каждый сервис с базой открывает два пула: владелец
// (миграции, служебные задачи) и роль данных (запросы). Сервис, который
// применяет миграции, но подключается одной ролью через pg.Connect, отдаёт
// коду запросов права владельца — так было с identity (дефект D14), и тесты
// этого не видели, потому что сами шли под ролью данных.
func TestServicesUseTwoRoles(t *testing.T) {
	mains, err := filepath.Glob("cmd/*/main.go")
	if err != nil || len(mains) == 0 {
		t.Fatalf("no service entry points found: %v", err)
	}
	withDB := 0
	for _, path := range mains {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if !strings.Contains(src, "migrations.FS") {
			continue // сервис без базы (gateway)
		}
		withDB++
		if !strings.Contains(src, "svc.Boot(") && !strings.Contains(src, "pg.OpenPools(") {
			t.Errorf("%s applies migrations but does not open two pools (svc.Boot or pg.OpenPools)", path)
		}
		if strings.Contains(src, "pg.Connect(") {
			t.Errorf("%s connects with a single role via pg.Connect", path)
		}
	}
	if withDB != 5 {
		t.Errorf("expected 5 services with a database, found %d", withDB)
	}
}
