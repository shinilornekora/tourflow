package booking_test

// РХД ПР-3: эксперимент с аномалией и четыре механизма защиты.
//
// Задача одна: 25 транзакций одновременно берут единственный свободный
// номер (inventory.booked: 0 → не больше 1). Сначала воспроизводится
// аномалия — наивный «прочитал, проверил, записал» на READ COMMITTED даёт
// овербукинг. Затем та же гонка проходит четырьмя способами; тест
// проверяет инвариант и печатает сравнение (go test -v -run Experiment).
// В сервисе выбрана блокировка строк (T31): бронь на N ночей блокирует N
// строк в порядке дат, а условная запись и CAS хороши только для одной
// строки.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

const racers = 25

type attemptResult struct {
	won     bool
	retries int
}

type mechanism struct {
	name string
	// take пытается занять номер; barrier.Wait() вызывается между чтением и
	// записью там, где механизм это допускает, чтобы гонка была честной.
	take func(ctx context.Context, e *env, barrier func()) (attemptResult, error)
}

func (e *env) resetInventory(t *testing.T) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(), `DELETE FROM inventory;
		INSERT INTO inventory(room_type_id, day, booked) VALUES ('`+e.room.String()+`', '2026-12-20', 0)`); err != nil {
		t.Fatal(err)
	}
}

const (
	qRead   = `SELECT booked FROM inventory WHERE room_type_id=$1 AND day='2026-12-20'`
	qWrite  = `UPDATE inventory SET booked=$2 WHERE room_type_id=$1 AND day='2026-12-20'`
	qCAS    = `UPDATE inventory SET booked=$2 WHERE room_type_id=$1 AND day='2026-12-20' AND booked=$3`
	qAtomic = `UPDATE inventory SET booked=booked+1 WHERE room_type_id=$1 AND day='2026-12-20' AND booked < $2`
)

func mechanisms() []mechanism {
	return []mechanism{
		{"0. наивно: чтение → проверка → запись, READ COMMITTED (аномалия)", func(ctx context.Context, e *env, barrier func()) (attemptResult, error) {
			var r attemptResult
			err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
				var booked int
				if err := tx.QueryRow(ctx, qRead, e.room).Scan(&booked); err != nil {
					return err
				}
				barrier() // все прочитали «свободно» — и все пишут
				if booked >= 1 {
					return nil
				}
				_, err := tx.Exec(ctx, qWrite, e.room, booked+1)
				r.won = err == nil
				return err
			})
			return r, err
		}},
		{"1. блокировка строки: SELECT … FOR UPDATE", func(ctx context.Context, e *env, barrier func()) (attemptResult, error) {
			var r attemptResult
			barrier()
			err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
				var booked int
				if err := tx.QueryRow(ctx, qRead+` FOR UPDATE`, e.room).Scan(&booked); err != nil {
					return err
				}
				if booked >= 1 {
					return nil
				}
				_, err := tx.Exec(ctx, qWrite, e.room, booked+1)
				r.won = err == nil
				return err
			})
			return r, err
		}},
		{"2. оптимистично: запись, только если значение не изменилось (CAS)", func(ctx context.Context, e *env, barrier func()) (attemptResult, error) {
			var r attemptResult
			first := true
			for {
				var booked int
				if err := e.pool.QueryRow(ctx, qRead, e.room).Scan(&booked); err != nil {
					return r, err
				}
				if first {
					barrier()
					first = false
				}
				if booked >= 1 {
					return r, nil
				}
				tag, err := e.pool.Exec(ctx, qCAS, e.room, booked+1, booked)
				if err != nil {
					return r, err
				}
				if tag.RowsAffected() == 1 {
					r.won = true
					return r, nil
				}
				r.retries++ // кто-то успел раньше — перечитать
			}
		}},
		{"3. условная атомарная запись: UPDATE … WHERE booked < total", func(ctx context.Context, e *env, barrier func()) (attemptResult, error) {
			var r attemptResult
			barrier()
			tag, err := e.pool.Exec(ctx, qAtomic, e.room, 1)
			r.won = err == nil && tag.RowsAffected() == 1
			return r, err
		}},
		{"4. SERIALIZABLE: наивный код + повтор при 40001", func(ctx context.Context, e *env, barrier func()) (attemptResult, error) {
			var r attemptResult
			first := true
			for {
				r.won = false
				err := pgx.BeginTxFunc(ctx, e.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
					var booked int
					if err := tx.QueryRow(ctx, qRead, e.room).Scan(&booked); err != nil {
						return err
					}
					if first {
						barrier()
						first = false
					}
					if booked >= 1 {
						return nil
					}
					_, err := tx.Exec(ctx, qWrite, e.room, booked+1)
					r.won = err == nil
					return err
				})
				if pg.IsRetryable(err) {
					r.retries++
					continue
				}
				return r, err
			}
		}},
	}
}

func TestConcurrencyExperiment(t *testing.T) {
	e := setup(t, 1)
	ctx := context.Background()
	t.Logf("%-70s %7s %8s %8s %9s", "механизм", "успехов", "booked", "повторов", "время")
	for i, m := range mechanisms() {
		e.resetInventory(t)
		var wins, retries atomic.Int32
		var ready, done sync.WaitGroup
		ready.Add(racers)
		gate := make(chan struct{})
		// barrier: каждый участник отмечается и ждёт, пока отметятся все.
		barrier := func() {
			ready.Done()
			<-gate
		}
		go func() { ready.Wait(); close(gate) }()
		start := time.Now()
		for g := 0; g < racers; g++ {
			done.Add(1)
			go func() {
				defer done.Done()
				r, err := m.take(ctx, e, barrier)
				if err != nil {
					t.Errorf("%s: %v", m.name, err)
				}
				if r.won {
					wins.Add(1)
				}
				retries.Add(int32(r.retries))
			}()
		}
		done.Wait()
		elapsed := time.Since(start)
		var booked int
		if err := e.owner.QueryRow(ctx, qRead, e.room).Scan(&booked); err != nil {
			t.Fatal(err)
		}
		t.Logf("%-70s %7d %8d %8d %9s", m.name, wins.Load(), booked, retries.Load(), elapsed.Round(time.Millisecond))
		if i == 0 {
			// Аномалия: «успехов» больше одного — один номер продан многим
			// (потерянная проверка). Счётчик при этом равен 1: записи затёрли
			// друг друга, и по остаткам овербукинг даже не виден.
			if wins.Load() <= 1 {
				t.Fatalf("the naive variant must oversell (lost check), got %d winners", wins.Load())
			}
			continue
		}
		if wins.Load() != 1 || booked != 1 {
			t.Fatalf("%s: winners=%d booked=%d, want exactly 1 and 1", m.name, wins.Load(), booked)
		}
	}
}
