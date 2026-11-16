#!/usr/bin/env bash
# Сквозной сценарий через gateway. Нужны curl и jq.
#   ./scripts/e2e.sh [http://localhost:8080]
# Проверяет: роли, назначение менеджера, доставку событий catalog → booking,
# гонку за последний номер (10 параллельных запросов), идемпотентность, изоляцию данных, отмену,
# компенсацию при отключении отеля и уведомления.
set -euo pipefail
API="${1:-http://localhost:8080}/api"
pass=0
ok()   { pass=$((pass+1)); printf '  \033[32m✔\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✘ %s\033[0m\n' "$1"; exit 1; }
req()  { # METHOD PATH TOKEN [BODY] [extra curl args...]
  local m=$1 p=$2 t=$3 b=${4:-}; shift 4 || shift $#
  curl -s -o /tmp/e2e.body -w '%{http_code}' -X "$m" "$API$p" \
    -H 'Content-Type: application/json' ${t:+-H "Authorization: Bearer $t"} ${b:+-d "$b"} "$@"
}
body() { cat /tmp/e2e.body; }
expect() { [[ "$1" == "$2" ]] && ok "$3" || { echo; body; echo; fail "$3: HTTP $1, ждали $2"; }; }
day() { date -u -d "+$1 day" +%F; }
login() { req POST /auth/login "" "{\"email\":\"$1\",\"password\":\"$2\"}" >/dev/null; body | jq -r .access_token; }
register() { req POST /auth/register "" "{\"email\":\"$1\",\"password\":\"Secret123\",\"full_name\":\"$2\"}"; }

RUN=$RANDOM$RANDOM
echo "== Пользователи и роли"
ADMIN=$(login admin@tourflow.local Admin12345); [[ $ADMIN != null ]] && ok "вход администратора" || fail "вход администратора"
expect "$(register "alice$RUN@mail.ru" 'Алиса')" 201 "регистрация клиента Алисы"; ALICE=$(body | jq -r .access_token)
expect "$(register "bob$RUN@mail.ru" 'Борис')" 201 "регистрация клиента Бориса"; BOB=$(body | jq -r .access_token)
expect "$(register "mgr$RUN@mail.ru" 'Мария')" 201 "регистрация будущего менеджера"; MGR_ID=$(body | jq -r .user.id)
expect "$(req PATCH /users/$MGR_ID/role "$ALICE" '{"role":"admin"}')" 403 "клиент не может выдавать роли"
expect "$(req PATCH /users/$MGR_ID/role "$ADMIN" '{"role":"hotel_manager"}')" 200 "админ выдаёт роль менеджера"
MGR=$(login "mgr$RUN@mail.ru" Secret123)

echo "== Каталог"
expect "$(req POST /hotels "$MGR" '{"name":"Тест","city":"Казань","stars":4}')" 403 "менеджер не создаёт отели"
expect "$(req POST /hotels "$ADMIN" "{\"name\":\"Отель $RUN\",\"city\":\"Казань\",\"stars\":4,\"address\":\"ул. Баумана, 1\"}")" 201 "админ создаёт отель"
HOTEL=$(body | jq -r .id)
expect "$(req POST /hotels/$HOTEL/managers "$ADMIN" "{\"user_id\":\"$MGR_ID\"}")" 200 "админ назначает менеджера (синхронная проверка роли в identity)"
expect "$(req POST /hotels/$HOTEL/room-types "$MGR" '{"name":"Стандарт","capacity":2,"total_rooms":1,"price":{"amount":450000,"currency":"RUB"},"amenities":["Wi-Fi"]}')" 201 "менеджер добавляет категорию (1 номер)"
ROOM=$(body | jq -r .id)
# Название в запросе: в базе могут быть сотни отелей Казани (после нагрузочного замера),
# и без него отель мог бы не попасть на первую страницу.
expect "$(req GET "/hotels?city=казань&q=$RUN&sort=price" "" "")" 200 "публичный поиск отелей"
[[ $(body | jq --arg h "$HOTEL" '[.items[]|select(.id==$h)]|length') == 1 ]] && ok "отель найден по городу без учёта регистра и части названия" || fail "поиск"

echo "== Доставка событий catalog → Kafka → booking"
IN=$(day 10); OUT=$(day 12)
for i in $(seq 1 40); do
  req GET "/availability?hotel_id=$HOTEL&check_in=$IN&check_out=$OUT&guests=2" "" "" >/dev/null
  [[ $(body | jq '.items|length') == 1 ]] && break; sleep 0.5
done
[[ $(body | jq '.items|length') == 1 ]] && ok "реплика каталога в booking обновилась (через ~$((i/2)) с)" || fail "событие не дошло"
[[ $(body | jq '.items[0].total_price.amount') == 900000 ]] && ok "цена за 2 ночи посчитана: 9000,00 ₽" || fail "цена"

echo "== Бронирование"
BOOK="{\"room_type_id\":\"$ROOM\",\"check_in\":\"$IN\",\"check_out\":\"$OUT\",\"guests\":2}"
expect "$(req POST /reservations "$ALICE" "$BOOK")" 422 "без Idempotency-Key запрос отклонён"
expect "$(req POST /reservations "$ADMIN" "$BOOK" -H "Idempotency-Key: admin-$RUN")" 403 "админ не бронирует"
expect "$(req POST /reservations "$ALICE" "$BOOK" -H "Idempotency-Key: alice-$RUN")" 201 "Алиса бронирует последний номер"
RES=$(body | jq -r .id)
code=$(curl -s -D /tmp/e2e.h -o /tmp/e2e.body -w '%{http_code}' -X POST "$API/reservations" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $ALICE" -H "Idempotency-Key: alice-$RUN" -d "$BOOK")
[[ $code == 200 && $(body | jq -r .id) == "$RES" ]] && grep -qi 'Idempotent-Replayed: true' /tmp/e2e.h \
  && ok "повтор с тем же ключом вернул ту же бронь (200, Idempotent-Replayed)" || fail "идемпотентность"
expect "$(req POST /reservations "$BOB" "$BOOK" -H "Idempotency-Key: bob-$RUN")" 409 "Борису номер на эти даты уже не достался"
[[ $(body | jq -r .error.code) == no_availability ]] && ok "код ошибки no_availability" || fail "код ошибки"
req GET "/availability?hotel_id=$HOTEL&check_in=$IN&check_out=$OUT" "" "" >/dev/null
[[ $(body | jq '.items|length') == 0 ]] && ok "клиент не видит занятую на эти даты категорию" || fail "доступность"

echo "== Изоляция данных"
expect "$(req GET /reservations/$RES "$BOB" "")" 404 "Борис не видит бронь Алисы"
expect "$(req GET "/reservations?hotel_id=$HOTEL" "$MGR" "")" 200 "менеджер видит брони своего отеля"
[[ $(body | jq .total) == 1 ]] && ok "у менеджера ровно 1 бронь" || fail "список менеджера"
expect "$(req GET /reservations/my "$ALICE" "")" 200 "«корзина» Алисы"
[[ $(body | jq .total) == 1 ]] && ok "в корзине 1 бронь" || fail "корзина"

echo "== Гонка за последний номер: 10 одновременных запросов"
RIN=$(day 20); ROUT=$(day 22)
RBOOK="{\"room_type_id\":\"$ROOM\",\"check_in\":\"$RIN\",\"check_out\":\"$ROUT\",\"guests\":2}"
race() { # номер запроса → HTTP-код; чётные — Алиса, нечётные — Борис, ключи разные
  local t=$ALICE; (( $1 % 2 )) && t=$BOB
  curl -s -o /dev/null -w '%{http_code}\n' -X POST "$API/reservations" -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $t" -H "Idempotency-Key: race-$RUN-$1" -d "$RBOOK"
}
export -f race; export API ALICE BOB RUN RBOOK
codes=$(seq 1 10 | xargs -P 10 -I{} bash -c 'race {}' | sort | uniq -c | tr -s ' ' | tr '\n' ';')
[[ $(grep -o ' 1 201' <<<" $codes" | wc -l) == 1 && $(grep -o '9 409' <<<"$codes" | wc -l) == 1 ]] \
  && ok "из 10 параллельных броней одного номера прошла ровно одна (201), девять получили 409" || fail "гонка: $codes"
expect "$(req GET "/reservations?hotel_id=$HOTEL&status=confirmed" "$MGR" "")" 200 "менеджер перечитывает брони"
[[ $(body | jq .total) == 2 ]] && ok "после гонки у отеля 2 подтверждённые брони, не больше" || fail "овербукинг: $(body | jq .total)"

echo "== Уведомления (booking → Kafka → notification)"
for i in $(seq 1 40); do req GET /notifications/unread-count "$ALICE" "" >/dev/null; [[ $(body | jq .unread) -ge 1 ]] && break; sleep 0.5; done
[[ $(body | jq .unread) -ge 1 ]] && ok "Алисе пришло уведомление о подтверждении" || fail "уведомление"

echo "== Отмена и возврат номера в продажу"
expect "$(req POST /reservations/$RES/cancel "$ALICE" '{}')" 200 "Алиса отменяет бронь"
expect "$(req POST /reservations "$BOB" "$BOOK" -H "Idempotency-Key: bob2-$RUN")" 201 "теперь номер достаётся Борису"
BOBRES=$(body | jq -r .id)

echo "== Компенсация: отключение отеля отменяет будущие брони"
req GET /hotels/$HOTEL "$ADMIN" "" >/dev/null; V=$(body | jq .hotel.version)
expect "$(req PATCH /hotels/$HOTEL/status "$MGR" "{\"status\":\"inactive\",\"version\":$V}")" 403 "менеджер не может отключить отель"
expect "$(req PATCH /hotels/$HOTEL/status "$ADMIN" "{\"status\":\"inactive\",\"version\":$V}")" 200 "админ отключает отель"
for i in $(seq 1 40); do req GET /reservations/$BOBRES "$BOB" "" >/dev/null; [[ $(body | jq -r .status) == cancelled ]] && break; sleep 0.5; done
[[ $(body | jq -r .status) == cancelled ]] && ok "бронь Бориса отменена системой: $(body | jq -r .cancel_reason)" || fail "компенсация"
for i in $(seq 1 40); do req GET "/notifications" "$BOB" "" >/dev/null; [[ $(body | jq '[.items[]|select(.kind=="reservation_cancelled")]|length') -ge 1 ]] && break; sleep 0.5; done
[[ $(body | jq '[.items[]|select(.kind=="reservation_cancelled")]|length') -ge 1 ]] && ok "Борису пришло уведомление об отмене" || fail "уведомление об отмене"
expect "$(req GET /hotels/$HOTEL "" "")" 404 "отключённый отель скрыт из публичного каталога"

echo "== Аудит"
expect "$(req GET "/audit?hotel_id=$HOTEL" "$MGR" "")" 200 "менеджер читает журнал аудита"
[[ $(body | jq .total) -ge 4 ]] && ok "в журнале $(body | jq .total) записей: создание и отмены" || fail "аудит"
[[ $(body | jq '[.items[]|select(.action=="hotel.deactivation_processed")]|length') == 1 ]] \
  && ok "в аудите есть след компенсации: отменено броней $(body | jq '[.items[]|select(.action=="hotel.deactivation_processed")][0].details.cancelled_reservations')" || fail "след компенсации"

echo "== Gateway"
expect "$(req GET /reservations/my "not-a-token" "")" 401 "мусорный токен отклонён на входе"
expect "$(curl -s -o /dev/null -w '%{http_code}' "${API%/api}/internal/users/$MGR_ID")" 404 "/internal/* не опубликован наружу"

echo; printf '\033[32mВсе проверки пройдены: %d\033[0m\n' "$pass"
