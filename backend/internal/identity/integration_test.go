package identity_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"sync"
	"testing"
	"time"

	"github.com/shinilornekora/tourflow/backend/internal/identity/app"
	"github.com/shinilornekora/tourflow/backend/internal/identity/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/identity/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

func setup(t *testing.T) (*app.Service, *auth.Verifier) {
	pool := pgtest.Fresh(t, "identity_test", migrations.FS)
	signer, err := auth.NewSigner("", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{Users: store.Users{Pool: pool}, Tokens: store.Tokens{Pool: pool}, Hasher: store.Bcrypt{Cost: 4}, Issuer: signer}
	return svc, auth.NewVerifier(auth.StaticKeys(signer.PublicKeys()))
}

func TestRegisterLoginAndToken(t *testing.T) {
	svc, verifier := setup(t)
	ctx := context.Background()
	s, err := svc.Register(ctx, app.RegisterInput{Email: "  Ivan@Example.COM ", Password: "secret123", FullName: "Иван  Петров"})
	if err != nil {
		t.Fatal(err)
	}
	if s.User.Email.String() != "ivan@example.com" || s.User.Profile.FullName != "Иван Петров" || s.User.Role != auth.RoleCustomer {
		t.Fatalf("normalization or role is wrong: %+v", s.User)
	}
	p, err := verifier.Verify(ctx, s.AccessToken)
	if err != nil || p.UserID != s.User.ID || p.Role != auth.RoleCustomer {
		t.Fatalf("issued token must verify: %+v %v", p, err)
	}
	if _, err := svc.Register(ctx, app.RegisterInput{Email: "ivan@example.com", Password: "secret123", FullName: "Дубль"}); apperr.As(err).Code != "email_taken" {
		t.Fatalf("duplicate email must conflict, got %v", err)
	}
	if _, err := svc.Login(ctx, "ivan@example.com", "wrong-pass1"); !apperr.Is(err, apperr.KindUnauthorized) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := svc.Login(ctx, "nobody@example.com", "secret123"); !apperr.Is(err, apperr.KindUnauthorized) {
		t.Fatalf("unknown email gives the same error: %v", err)
	}
	if _, err := svc.Login(ctx, "IVAN@example.com", "secret123"); err != nil {
		t.Fatalf("email is case-insensitive: %v", err)
	}
	if _, err := svc.Register(ctx, app.RegisterInput{Email: "bad", Password: "short", FullName: ""}); len(apperr.As(err).Fields) != 3 {
		t.Fatalf("all three fields must be reported at once, got %+v", apperr.As(err).Fields)
	}
}

// Ротация refresh-токена и обнаружение кражи: повторное предъявление уже
// использованного токена отзывает всю цепочку, включая свежий токен.
func TestRefreshRotationAndReuseDetection(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	s0, err := svc.Register(ctx, app.RegisterInput{Email: "a@b.ru", Password: "secret123", FullName: "Анна"})
	if err != nil {
		t.Fatal(err)
	}
	s1, err := svc.Refresh(ctx, s0.RefreshToken)
	if err != nil || s1.RefreshToken == s0.RefreshToken {
		t.Fatalf("refresh must rotate: %v", err)
	}
	// Злоумышленник предъявляет старый токен.
	if _, err := svc.Refresh(ctx, s0.RefreshToken); !apperr.Is(err, apperr.KindUnauthorized) {
		t.Fatalf("reused token must be rejected, got %v", err)
	}
	// И легитимный свежий токен тоже больше не работает.
	if _, err := svc.Refresh(ctx, s1.RefreshToken); !apperr.Is(err, apperr.KindUnauthorized) {
		t.Fatalf("whole family must be revoked after reuse, got %v", err)
	}
}

func TestParallelRefreshGivesOneWinner(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	s0, err := svc.Register(ctx, app.RegisterInput{Email: "p@b.ru", Password: "secret123", FullName: "Пётр"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Refresh(ctx, s0.RefreshToken); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok > 1 {
		t.Fatalf("one refresh token produced %d sessions", ok)
	}
}

func TestRolesAndAdmin(t *testing.T) {
	svc, _ := setup(t)
	ctx := context.Background()
	created, err := svc.EnsureAdmin(ctx, "admin@tourflow.local", "admin12345", "Админ")
	if err != nil || !created {
		t.Fatalf("bootstrap admin: %v", err)
	}
	again, _ := svc.EnsureAdmin(ctx, "admin2@tourflow.local", "admin12345", "Админ")
	if again {
		t.Fatal("second admin must not be bootstrapped")
	}
	adminSession, err := svc.Login(ctx, "admin@tourflow.local", "admin12345")
	if err != nil {
		t.Fatal(err)
	}
	admin := auth.WithPrincipal(ctx, auth.Principal{UserID: adminSession.User.ID, Role: auth.RoleAdmin})
	u, err := svc.Register(ctx, app.RegisterInput{Email: "m@h.ru", Password: "secret123", FullName: "Менеджер"})
	if err != nil {
		t.Fatal(err)
	}
	customer := auth.WithPrincipal(ctx, auth.Principal{UserID: u.User.ID, Role: auth.RoleCustomer})
	if _, err := svc.ChangeRole(customer, u.User.ID, auth.RoleAdmin); !apperr.Is(err, apperr.KindForbidden) {
		t.Fatalf("customer cannot grant roles: %v", err)
	}
	m, err := svc.ChangeRole(admin, u.User.ID, auth.RoleManager)
	if err != nil || m.Role != auth.RoleManager || m.Version != 2 {
		t.Fatalf("admin grants manager: %+v %v", m, err)
	}
	if _, err := svc.Refresh(ctx, u.RefreshToken); !apperr.Is(err, apperr.KindUnauthorized) {
		t.Fatalf("role change must end old sessions: %v", err)
	}
	if _, err := svc.ChangeRole(admin, adminSession.User.ID, auth.RoleCustomer); apperr.As(err).Code != "self_demotion" {
		t.Fatalf("admin cannot demote self: %v", err)
	}
	list, total, err := svc.ListUsers(admin, app.UserFilter{Role: auth.RoleManager, Limit: 10})
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("filter by role: total=%d err=%v", total, err)
	}
	// Оптимистическая блокировка профиля.
	if _, err := svc.UpdateMe(customer, "Новое Имя", "+7 900 000-00-00", 1); apperr.As(err).Code != "version_mismatch" {
		t.Fatalf("stale version must conflict: %v", err)
	}
}

// Запросы identity идут под ролью данных, как у остальных сервисов (T111):
// код запросов не может менять схему базы, где лежат хэши паролей и
// refresh-токены. Тест нужен после дефекта D14: тесты шли под ролью данных,
// а сам сервис — под владельцем.
func TestDataRoleCannotChangeSchema(t *testing.T) {
	if !pgtest.Split() {
		t.Skip("TEST_PG_APP_DSN не задан: роль данных совпадает с владельцем")
	}
	pool := pgtest.Fresh(t, "identity_test", migrations.FS)
	for _, q := range []string{
		`CREATE TABLE leak (id int)`,
		`ALTER TABLE users ADD COLUMN backdoor text`,
		`TRUNCATE users CASCADE`,
		`DROP TABLE refresh_tokens`,
		`ALTER TABLE users OWNER TO CURRENT_USER`,
	} {
		_, err := pool.Exec(context.Background(), q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Errorf("data role ran %q: %v", q, err)
		}
	}
}
