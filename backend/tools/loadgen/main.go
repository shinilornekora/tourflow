// loadgen — генератор нагрузки для паспорта РХД ПР-1 (docs/LOAD.md).
//
// Замер идёт сквозь весь путь запроса: gateway (JWT, лимиты, breaker) →
// booking → PostgreSQL (RLS, блокировки строк, outbox).
//
//	go run ./tools/loadgen -prepare state.json   # отели, категории, клиенты
//	go run ./tools/loadgen -state state.json -mix shop -workers 8 -requests 2000 -runs 3
//	go run ./tools/loadgen -state state.json -mix hot  -workers 8 -requests 1000 -runs 3
//
// Сценарии:
//
//	shop — 80% чтений GET /availability и 20% записей POST /reservations;
//	       отель выбирается по Зипфу (несколько отелей получают большую
//	       часть запросов), даты — случайные в ближайшие 300 суток.
//	hot  — только записи в ОДНУ категорию на пересекающиеся даты одного
//	       месяца: худший случай для блокировок строк остатков.
//
// Цикл замкнутый: воркер шлёт следующий запрос после ответа на предыдущий.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

type state struct {
	Hotels    []string `json:"hotels"`
	RoomTypes []string `json:"room_types"` // по одной категории на отель, тот же индекс
	Emails    []string `json:"emails"`
}

var (
	base     = flag.String("base", "http://localhost:8080", "gateway")
	identity = flag.String("identity", "http://localhost:8081", "identity напрямую — для регистрации и входа мимо лимита 5/мин")
	prepare  = flag.String("prepare", "", "создать данные и записать состояние в файл")
	stateF   = flag.String("state", "", "файл состояния")
	mix      = flag.String("mix", "shop", "shop | hot")
	workers  = flag.Int("workers", 8, "параллельных воркеров")
	requests = flag.Int("requests", 2000, "запросов на повтор (если -duration не задан)")
	duration = flag.Duration("duration", 0, "длительность повтора; задаёт установившийся режим вместо числа запросов")
	winStart = flag.Int("window-start", 20, "hot: первый день окна первого повтора (сутки от сегодня)")
	winDays  = flag.Int("window", 30, "hot: ширина окна в сутках; каждый повтор берёт своё окно")
	runs     = flag.Int("runs", 3, "повторов")
	hotels   = flag.Int("hotels", 20, "отелей (prepare)")
	users    = flag.Int("users", 200, "клиентов (prepare)")
	seed     = flag.Int64("seed", 1, "зерно ГСЧ: повторы воспроизводимы")
)

var client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

func call(method, url, token string, body any, hdr map[string]string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func must(code int, b []byte, err error, want int, what string) []byte {
	if err != nil || code != want {
		fmt.Fprintf(os.Stderr, "%s: HTTP %d %v %s\n", what, code, err, b)
		os.Exit(1)
	}
	return b
}

func login(email, password string) string {
	code, b, err := call("POST", *identity+"/api/auth/login", "", map[string]string{"email": email, "password": password}, nil)
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(must(code, b, err, 200, "login "+email), &out)
	return out.AccessToken
}

func doPrepare() {
	admin := login("admin@tourflow.local", "Admin12345")
	run := uuid.NewString()[:8]
	var st state
	for i := 0; i < *hotels; i++ {
		code, b, err := call("POST", *base+"/api/hotels", admin, map[string]any{
			"name": fmt.Sprintf("Нагрузка %s-%02d", run, i), "city": "Казань", "stars": 4, "address": "ул. Замера, 1"}, nil)
		var h struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(must(code, b, err, 201, "create hotel"), &h)
		code, b, err = call("POST", *base+"/api/hotels/"+h.ID+"/room-types", admin, map[string]any{
			"name": "Стандарт", "capacity": 2, "total_rooms": 1000, "price": map[string]any{"amount": 450000, "currency": "RUB"}}, nil)
		var r struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(must(code, b, err, 201, "create room type"), &r)
		st.Hotels, st.RoomTypes = append(st.Hotels, h.ID), append(st.RoomTypes, r.ID)
		time.Sleep(50 * time.Millisecond) // общий лимит gateway
	}
	for i := 0; i < *users; i++ {
		email := fmt.Sprintf("load-%s-%04d@tourflow.local", run, i)
		code, b, err := call("POST", *identity+"/api/auth/register", "", map[string]string{
			"email": email, "password": "Secret123", "full_name": "Нагрузка"}, nil)
		must(code, b, err, 201, "register")
		st.Emails = append(st.Emails, email)
	}
	// Ждём, пока реплика каталога в booking догонит последний отель.
	last := len(st.Hotels) - 1
	in, out := time.Now().AddDate(0, 0, 5).Format("2006-01-02"), time.Now().AddDate(0, 0, 6).Format("2006-01-02")
	for i := 0; ; i++ {
		_, b, _ := call("GET", fmt.Sprintf("%s/api/availability?hotel_id=%s&check_in=%s&check_out=%s", *base, st.Hotels[last], in, out), "", nil, nil)
		var a struct {
			Items []any `json:"items"`
		}
		_ = json.Unmarshal(b, &a)
		if len(a.Items) == 1 {
			break
		}
		if i > 120 {
			fmt.Fprintln(os.Stderr, "replica did not catch up")
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(st, "", " ")
	if err := os.WriteFile(*prepare, b, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("prepared: %d hotels, %d customers → %s\n", len(st.Hotels), len(st.Emails), *prepare)
}

type sample struct {
	op  string
	ms  float64
	res string // ok | sold_out | error
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p+0.999999) - 1 // ближайший ранг
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

func main() {
	flag.Parse()
	if *prepare != "" {
		doPrepare()
		return
	}
	raw, err := os.ReadFile(*stateF)
	if err != nil {
		panic(err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		panic(err)
	}
	tokens := make([]string, len(st.Emails))
	for i, e := range st.Emails {
		tokens[i] = login(e, "Secret123")
	}
	// Прогрев: по чтению на отель — соединения пулов и JWKS.
	in0, out0 := time.Now().AddDate(0, 0, 3).Format("2006-01-02"), time.Now().AddDate(0, 0, 4).Format("2006-01-02")
	for _, h := range st.Hotels {
		_, _, _ = call("GET", fmt.Sprintf("%s/api/availability?hotel_id=%s&check_in=%s&check_out=%s", *base, h, in0, out0), "", nil, nil)
	}
	fmt.Printf("# loadgen mix=%s workers=%d requests=%d duration=%s runs=%d hotels=%d customers=%d go=%s cpu=%d date=%s\n",
		*mix, *workers, *requests, *duration, *runs, len(st.Hotels), len(tokens), runtime.Version(), runtime.NumCPU(), time.Now().UTC().Format(time.RFC3339))

	for run := 1; run <= *runs; run++ {
		jobs := make(chan int)
		out := make([][]sample, *workers)
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				rnd := rand.New(rand.NewSource(*seed*1000 + int64(run)*100 + int64(w)))
				zipf := rand.NewZipf(rnd, 1.2, 1, uint64(len(st.Hotels)-1))
				for i := range jobs {
					var s sample
					h := int(zipf.Uint64())
					day := 1 + rnd.Intn(300)
					nights := 1 + rnd.Intn(3)
					write := i%5 == 0
					if *mix == "hot" {
						// Одна категория, окно -window суток внутри залитых данных:
						// каждая бронь пересекается по строкам остатков со многими
						// параллельными. У каждого повтора своё окно той же ширины
						// (сдвиг на -window суток): повторы стартуют с одинакового
						// заполнения, и категория (1 000 номеров) не распродаётся.
						h, write = len(st.Hotels)-1, true
						day, nights = *winStart+(run-1)**winDays+rnd.Intn(*winDays-2), 1+rnd.Intn(3)
					}
					ci := time.Now().AddDate(0, 0, day).Format("2006-01-02")
					co := time.Now().AddDate(0, 0, day+nights).Format("2006-01-02")
					t0 := time.Now()
					if write {
						s.op = "book"
						code, _, err := call("POST", *base+"/api/reservations", tokens[i%len(tokens)],
							map[string]any{"room_type_id": st.RoomTypes[h], "check_in": ci, "check_out": co, "guests": 2},
							map[string]string{"Idempotency-Key": uuid.NewString()})
						switch {
						case err == nil && code == 201:
							s.res = "ok"
						case err == nil && code == 409:
							s.res = "sold_out"
						default:
							s.res = fmt.Sprintf("error:%d", code)
						}
					} else {
						s.op = "availability"
						code, _, err := call("GET", fmt.Sprintf("%s/api/availability?hotel_id=%s&check_in=%s&check_out=%s&guests=2", *base, st.Hotels[h], ci, co), "", nil, nil)
						s.res = "ok"
						if err != nil || code != 200 {
							s.res = fmt.Sprintf("error:%d", code)
						}
					}
					s.ms = float64(time.Since(t0).Microseconds()) / 1000
					out[w] = append(out[w], s)
				}
			}(w)
		}
		limit, n := 10*time.Minute, *requests
		if *duration > 0 {
			limit, n = *duration, 1<<31-1
		}
		ctx, cancel := context.WithTimeout(context.Background(), limit)
	feed:
		for i := 0; i < n; i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				break feed
			}
		}
		cancel()
		close(jobs)
		wg.Wait()
		wall := time.Since(start)
		byOp := map[string][]sample{}
		total := 0
		for _, ss := range out {
			for _, s := range ss {
				byOp[s.op] = append(byOp[s.op], s)
				total++
			}
		}
		fmt.Printf("run=%d workers=%d wall=%s total=%d throughput=%.0f/s\n", run, *workers, wall.Round(time.Millisecond), total, float64(total)/wall.Seconds())
		ops := make([]string, 0, len(byOp))
		for op := range byOp {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			ss := byOp[op]
			lat := make([]float64, 0, len(ss))
			res := map[string]int{}
			sum := 0.0
			for _, s := range ss {
				lat = append(lat, s.ms)
				sum += s.ms
				res[s.res]++
			}
			sort.Float64s(lat)
			errs := len(ss) - res["ok"] - res["sold_out"]
			fmt.Printf("  %-12s n=%-6d ok=%-6d sold_out=%-4d err=%-3d rate=%.0f/s mean=%.1f p50=%.1f p95=%.1f p99=%.1f max=%.1f ms\n",
				op, len(ss), res["ok"], res["sold_out"], errs, float64(len(ss))/wall.Seconds(), sum/float64(len(lat)),
				pct(lat, 0.50), pct(lat, 0.95), pct(lat, 0.99), lat[len(lat)-1])
			for k, v := range res {
				if k != "ok" && k != "sold_out" {
					fmt.Printf("    %s ×%d\n", k, v)
				}
			}
		}
	}
}
