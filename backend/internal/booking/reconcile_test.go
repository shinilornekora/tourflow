package booking_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/shinilornekora/tourflow/backend/internal/booking/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
)

// Фоновая сверка (РХД ПР-1, третий уровень защиты инварианта «остатки =
// подтверждённые брони»): на согласованных данных молчит, расхождение
// находит, записывает и не исправляет.
func TestReconcileFindsDrift(t *testing.T) {
	e := setup(t, 5)
	maint := store.Store{Pool: e.owner}
	ctx := context.Background()
	alice := uuid.New()
	r, _, err := e.book(alice, 2, 5, "key-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.book(uuid.New(), 4, 6, "key-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Cancel(as(alice, auth.RoleCustomer), r.ID, ""); err != nil {
		t.Fatal(err)
	}
	if bad, err := maint.Reconcile(ctx); err != nil || len(bad) != 0 {
		t.Fatalf("consistent data must have no drift: %+v %v", bad, err)
	}
	// Порча: остаток уехал вверх на одни сутки, а на другие строка пропала.
	if _, err := e.owner.Exec(ctx, `UPDATE inventory SET booked = booked + 2 WHERE day = '2026-12-05'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.owner.Exec(ctx, `DELETE FROM inventory WHERE day = '2026-12-06'`); err != nil {
		t.Fatal(err)
	}
	bad, err := maint.Reconcile(ctx)
	if err != nil || len(bad) != 2 {
		t.Fatalf("two mismatches expected: %+v %v", bad, err)
	}
	if bad[0].Booked != 3 || bad[0].Confirmed != 1 || bad[1].Booked != 0 || bad[1].Confirmed != 1 {
		t.Fatalf("mismatch details: %+v", bad)
	}
	var logged int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM reconciliation_issues`).Scan(&logged); err != nil || logged != 2 {
		t.Fatalf("issues must be recorded: %d %v", logged, err)
	}
}
