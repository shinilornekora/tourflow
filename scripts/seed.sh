#!/usr/bin/env bash
# Демо-данные: отели в нескольких городах, менеджер, клиент.
#   ./scripts/seed.sh [http://localhost:8080]
# Повторный запуск безопасен: занятые email и дубли просто пропускаются.
set -uo pipefail
API="${1:-http://localhost:8080}/api"
j() { curl -s -X "$1" "$API$2" -H 'Content-Type: application/json' ${3:+-H "Authorization: Bearer $3"} ${4:+-d "$4"}; }

# Ждём, пока поднимутся gateway и identity (до 3 минут: в docker compose
# сид стартует вместе с системой).
# Проверяем GET-запросами: попытки входа ограничены лимитом (5 в минуту).
code() { curl -s -o /dev/null -w '%{http_code}' "$API$1"; }
for _ in $(seq 1 90); do
  [[ $(code /users/me) == 401 && $(code /cities) == 200 ]] && break
  sleep 2
done
ADMIN=$(j POST /auth/login "" '{"email":"admin@tourflow.local","password":"Admin12345"}' | jq -r .access_token)
[[ $ADMIN == null || -z $ADMIN ]] && { echo "admin login failed — система запущена?"; exit 1; }

# Отели уже есть — сид выполнялся раньше, повторно не создаём.
if [[ $(j GET "/hotels?include_inactive=true&page_size=1" "$ADMIN" | jq -r .total) != 0 ]]; then
  echo "Демо-данные уже загружены."
  exit 0
fi

j POST /auth/register "" '{"email":"manager@tourflow.local","password":"Manager123","full_name":"Мария Соколова","phone":"+7 900 111-22-33"}' >/dev/null
j POST /auth/register "" '{"email":"client@tourflow.local","password":"Client123","full_name":"Иван Петров","phone":"+7 900 444-55-66"}' >/dev/null
MGR_ID=$(j GET "/users?q=manager@tourflow.local" "$ADMIN" | jq -r '.items[0].id')
j PATCH "/users/$MGR_ID/role" "$ADMIN" '{"role":"hotel_manager"}' >/dev/null

hotel() { # name city stars address description
  j POST /hotels "$ADMIN" "$(jq -nc --arg n "$1" --arg c "$2" --argjson s "$3" --arg a "$4" --arg d "$5" \
    '{name:$n,city:$c,stars:$s,address:$a,description:$d,photo_url:""}')" | jq -r .id
}
room() { # hotel name capacity total price_rub amenities_json description
  j POST "/hotels/$1/room-types" "$ADMIN" "$(jq -nc --arg n "$2" --argjson c "$3" --argjson t "$4" --argjson p "$5" --argjson a "$6" --arg d "$7" \
    '{name:$n,capacity:$c,total_rooms:$t,price:{amount:($p*100),currency:"RUB"},amenities:$a,description:$d,photo_url:""}')" >/dev/null
}

H=$(hotel "Казанские купола" "Казань" 4 "ул. Баумана, 12" "Пять минут пешком до Кремля. Панорамный завтрак на крыше и тихие номера окнами во двор.")
room "$H" "Стандарт" 2 12 5200 '["Wi-Fi","Кондиционер","Завтрак"]' "Двуспальная кровать, рабочее место, душевая."
room "$H" "Семейный" 4 4 8900 '["Wi-Fi","Кондиционер","Завтрак","Диван-кровать"]' "Две комнаты, подойдёт для семьи с детьми."
room "$H" "Люкс с видом на Кремль" 2 1 15400 '["Wi-Fi","Вид на Кремль","Ванна","Мини-бар"]' "Единственный номер с панорамными окнами на Кремль."
j POST "/hotels/$H/managers" "$ADMIN" "{\"user_id\":\"$MGR_ID\"}" >/dev/null

H=$(hotel "Волжская пристань" "Казань" 3 "Набережная, 3" "Недорогой отель у воды: велопрокат, прогулки по набережной.")
room "$H" "Эконом" 1 8 2900 '["Wi-Fi"]' "Компактный одноместный номер."
room "$H" "Стандарт" 2 10 3900 '["Wi-Fi","Вид на Волгу"]' ""

H=$(hotel "Невский берег" "Санкт-Петербург" 5 "наб. реки Мойки, 48" "Особняк XIX века в центре. Камин в лобби, библиотека, спа.")
room "$H" "Классический" 2 15 11800 '["Wi-Fi","Ванна","Халаты","Мини-бар"]' "Высокие потолки, лепнина, окна на Мойку."
room "$H" "Апартаменты" 4 3 24500 '["Wi-Fi","Кухня","Ванна","Вид на реку"]' "Гостиная, спальня и кухня."
j POST "/hotels/$H/managers" "$ADMIN" "{\"user_id\":\"$MGR_ID\"}" >/dev/null

H=$(hotel "Белые ночи" "Санкт-Петербург" 3 "Лиговский пр., 10" "Рядом с Московским вокзалом — удобно, если вы на один день.")
room "$H" "Стандарт" 2 20 4600 '["Wi-Fi","Кондиционер"]' ""

H=$(hotel "Морской бриз" "Сочи" 4 "Курортный пр., 75" "Собственный пляж, бассейн с подогревом и детский клуб.")
room "$H" "Стандарт с балконом" 2 25 7300 '["Wi-Fi","Балкон","Кондиционер"]' ""
room "$H" "Семейный с видом на море" 4 6 13200 '["Wi-Fi","Вид на море","Балкон","Кондиционер"]' ""

H=$(hotel "Горный приют" "Сочи" 5 "Эсто-Садок, Красная Поляна" "Шале у подъёмников: ски-сервис, сауна, ресторан альпийской кухни.")
room "$H" "Шале" 6 4 32000 '["Wi-Fi","Камин","Сауна","Кухня"]' "Отдельный домик на шесть гостей."

H=$(hotel "Арбат Резиденс" "Москва" 4 "ул. Арбат, 27" "Пешеходный Арбат за дверью, до Кремля — 15 минут.")
room "$H" "Стандарт" 2 30 9100 '["Wi-Fi","Кондиционер","Сейф"]' ""
room "$H" "Бизнес" 2 10 12900 '["Wi-Fi","Рабочее место","Кофемашина"]' ""

echo "Готово. Входы:"
echo "  admin@tourflow.local / Admin12345"
echo "  manager@tourflow.local / Manager123  (менеджер «Казанских куполов» и «Невского берега»)"
echo "  client@tourflow.local / Client123"
