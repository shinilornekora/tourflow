# TourFlow — API gateway для фронтенда

Единственный адрес для SPA: **gateway**, `http://localhost:8080/api` (в docker compose фронтенд ходит туда же через свой nginx по `/api`). SPA не обращается к сервисам напрямую: gateway сам решает, какой сервис отвечает на запрос.

| Префикс | Метод | Сервис |
|---|---|---|
| `/api/auth/*`, `/api/users/*` | любой | identity |
| `/api/hotels/*`, `/api/room-types/*`, `/api/cities` | любой | catalog |
| `/api/reservations/*` | **GET** | **booking-query** (модель чтения) |
| `/api/reservations/*` | POST | booking (команды) |
| `/api/availability`, `/api/audit` | GET | booking |
| `/api/notifications/*` | любой | notification |

`/internal/*` снаружи недоступен (404).

## Общие правила

**Авторизация.** `Authorization: Bearer <access_token>`. Access-токен живёт 15 минут. Refresh-токен лежит в httpOnly-cookie `tf_refresh` (путь `/api/auth`, `SameSite=Strict`), поэтому запросы к `/api/auth/*` отправляйте с `credentials: 'include'`. Чтобы обновить сессию, вызовите `POST /api/auth/refresh` без тела. Мусорный или просроченный токен отклоняется прямо на gateway (401).

**Роли:** `customer` (клиент), `hotel_manager` (менеджер отеля), `admin`. Роль лежит в `user.role` ответа входа.

**Ошибки** приходят в одном формате:

```json
{ "error": { "code": "no_availability", "message": "Текст для пользователя", "fields": { "check_in": "..." }, "request_id": "..." } }
```

| HTTP | Когда |
|---|---|
| 401 | нет токена или он недействителен |
| 403 | роль не позволяет действие |
| 404 | ресурса нет **или он чужой** (чужая бронь неотличима от несуществующей) |
| 409 | конфликт: нет мест (`no_availability`), устаревшая версия (`version_mismatch`), бронь уже отменена (`already_cancelled`), поздно отменять (`too_late`) |
| 422 | ошибка валидации; `fields` — ошибки по полям, их можно показать прямо под полями формы |
| 429 | превышен лимит запросов (30 rps на IP; вход и регистрация — 5 в минуту), есть заголовок `Retry-After` |
| 503 | сервис временно недоступен (сработал circuit breaker, или сервис не ответил вовремя) |

**Пагинация:** `?page=1&page_size=10`, ответ: `{ items, page, page_size, total }`.

**Деньги** всегда передаются в копейках: `{ "amount": 450000, "currency": "RUB" }` означает 4500,00 ₽. Поддерживается только RUB.

**Даты:** `YYYY-MM-DD`. Проживание задаётся полуинтервалом `[check_in, check_out)`: день выезда уже свободен для следующего гостя.

**Оптимистическая блокировка.** У изменяемых сущностей есть поле `version`. Его нужно передать в PUT или PATCH; если кто-то изменил сущность раньше, придёт 409 `version_mismatch`.

### Согласованность чтения броней (важно для UI)

Команды (`POST /api/reservations`, `POST /api/reservations/{id}/cancel`) выполняет **booking**. Списки, карточки и сводки отдаёт **booking-query**, а он узнаёт о брони из события Kafka, обычно через сотни миллисекунд. Отсюда два правила для SPA:

1. Результат команды берите **из её ответа**: там бронь в актуальном состоянии. Например, после брони сразу покажите экран подтверждения по ответу POST и не перечитывайте карточку.
2. Списки после команды обновляйте оптимистично: вставьте или обновите элемент из ответа команды, а потом перезапросите список в фоне. Если `GET /api/reservations/{id}` сразу после брони вернул 404, повторите запрос через 300–500 мс. Номер версии в карточке (`version`) растёт с каждым изменением: подтверждённая бронь имеет версию 1, отменённая — 2.

---

## Identity: вход и пользователи

| Метод и путь | Кто | Тело / параметры | Ответ |
|---|---|---|---|
| `POST /api/auth/register` | все | `{ email, password, full_name, phone? }` | 201 `Session` + cookie; роль всегда `customer` |
| `POST /api/auth/login` | все | `{ email, password }` | 200 `Session` + cookie |
| `POST /api/auth/refresh` | все (с cookie) | — | 200 `Session` (refresh-токен ротируется) |
| `POST /api/auth/logout` | все | — | 204, cookie стёрта |
| `GET /api/users/me` | вошедший | — | `User` |
| `PATCH /api/users/me` | вошедший | `{ full_name, phone, version }` | `User` |
| `POST /api/users/me/password` | вошедший | `{ current_password, new_password }` | 204 |
| `GET /api/users` | admin | `?role=&q=&page=&page_size=` | `Paged<User>` |
| `PATCH /api/users/{id}/role` | admin | `{ role }` | `User` |

## Catalog: отели и категории номеров

| Метод и путь | Кто | Тело / параметры | Ответ |
|---|---|---|---|
| `GET /api/hotels` | все | `?q=&city=&stars_min=&guests=&price_max=&sort=&page=&page_size=` (по умолчанию 12 на странице); `sort`: `name`, `-stars`, `price`, `-price`, `-created_at`; `include_inactive=true` показывает отключённые отели: админу все, менеджеру только свои | `Paged<HotelListItem>` |
| `GET /api/hotels/managed` | manager, admin | те же фильтры | `Paged<HotelListItem>`: отели менеджера (для админа все) |
| `GET /api/hotels/{id}` | все | — | `{ hotel: Hotel, room_types: RoomType[] }`; отключённый отель видят только admin и его менеджеры |
| `POST /api/hotels` | admin | `HotelInput` | 201 `Hotel` |
| `PUT /api/hotels/{id}` | admin, менеджер отеля | `HotelInput` + `version` | `Hotel` |
| `PATCH /api/hotels/{id}/status` | admin | `{ status: "active" \| "inactive", version }` | `Hotel`. Отключение отеля **отменяет будущие брони** (асинхронно, гости получат уведомление) |
| `POST /api/hotels/{id}/managers` | admin | `{ user_id }` (у пользователя должна быть роль `hotel_manager`) | `Hotel` |
| `DELETE /api/hotels/{id}/managers/{userId}` | admin | — | `Hotel` |
| `POST /api/hotels/{id}/room-types` | admin, менеджер отеля | `RoomTypeInput` | 201 `RoomType` |
| `GET /api/room-types/{id}` | все | — | `{ room_type: RoomType, hotel: Hotel }` |
| `PUT /api/room-types/{id}` | admin, менеджер отеля | `RoomTypeInput` + `version` | `RoomType` |
| `PATCH /api/room-types/{id}/status` | admin, менеджер отеля | `{ status: "active" \| "archived", version }` | `RoomType` (архивная категория не продаётся, существующие брони сохраняются) |
| `GET /api/cities` | все | — | `{ items: string[] }` |

## Booking: доступность, команды, аудит

| Метод и путь | Кто | Тело / параметры | Ответ |
|---|---|---|---|
| `GET /api/availability` | все | `?hotel_id=&check_in=&check_out=&guests=` | `{ items: Availability[] }`. Клиент и аноним видят только категории со свободными номерами, менеджер и админ видят все |
| `POST /api/reservations` | customer | заголовок **`Idempotency-Key`** (8–128 символов, например `crypto.randomUUID()`), тело `{ room_type_id, check_in, check_out, guests }` | 201 `Reservation`. Повтор с тем же ключом даёт 200 и ту же бронь, с заголовком `Idempotent-Replayed: true`. Ошибка 409 `no_availability` означает, что мест нет |
| `POST /api/reservations/{id}/cancel` | клиент (своя бронь, до дня заезда); менеджер отеля и admin (до дня выезда, `reason` обязателен) | `{ reason? }` | `Reservation` со `status: "cancelled"` |
| `GET /api/audit` | manager (свои отели), admin | `?hotel_id=&page=&page_size=` (по умолчанию 20) | `Paged<AuditRecord>` |

Генерируйте `Idempotency-Key` один раз на попытку бронирования и используйте его же при повторе после сетевой ошибки. Так двойной клик или повтор запроса не создадут вторую бронь.

## Booking-query: карточки, списки, сводки

Все ответы этого раздела приходят из модели чтения (см. «Согласованность чтения броней» выше).

| Метод и путь | Кто | Параметры | Ответ |
|---|---|---|---|
| `GET /api/reservations/my` | customer | `?status=confirmed\|cancelled&from=&to=&page=&page_size=` (по умолчанию 10, максимум 50) | `Paged<Reservation>`: брони клиента для личного кабинета, текущие и история |
| `GET /api/reservations/my/summary` | customer | — | `CustomerSummary` |
| `GET /api/reservations` | manager (только свои отели), admin | `?hotel_id=&status=&from=&to=&page=&page_size=` | `Paged<Reservation>` |
| `GET /api/reservations/summary` | manager (свои отели), admin | `?hotel_id=&from=&to=` | `HotelSummary` |
| `GET /api/reservations/{id}` | владелец брони, менеджер её отеля, admin | — | `Reservation` |

Фильтр `from`/`to` выбирает проживания, пересекающиеся с периодом `[from, to)`. Списки отсортированы по дате заезда, от поздних к ранним.

Для экранов «История» и «Текущие» вызывайте `/api/reservations/my?status=confirmed` и смотрите на даты, либо возьмите счётчики из `/my/summary`.

## Notification: уведомления

| Метод и путь | Кто | Ответ |
|---|---|---|
| `GET /api/notifications?unread=true&page=&page_size=` | вошедший | `Paged<Notification>` |
| `GET /api/notifications/unread-count` | вошедший | `{ unread: number }`; для бейджа в шапке удобно опрашивать раз в 15–30 с |
| `POST /api/notifications/{id}/read` | вошедший | 204 |
| `POST /api/notifications/read-all` | вошедший | `{ marked: number }` |

---

## Типы (TypeScript)

```ts
export type Role = 'customer' | 'hotel_manager' | 'admin';
export interface Money { amount: number; currency: 'RUB' } // копейки
export interface Paged<T> { items: T[]; page: number; page_size: number; total: number }
export interface ApiError { error: { code: string; message: string; fields?: Record<string, string>; request_id?: string } }

export interface User {
  id: string; email: string; full_name: string; phone: string; role: Role;
  created_at: string; version: number;
}
export interface Session { access_token: string; expires_at: string; user: User }

export type HotelStatus = 'active' | 'inactive';
export interface Hotel {
  id: string; name: string; city: string; address: string; description: string;
  stars: number; photo_url: string; status: HotelStatus;
  manager_ids?: string[]; created_at: string; updated_at: string; version: number;
}
export interface HotelListItem extends Hotel { min_price?: Money; room_type_count: number }
export interface HotelInput {
  name: string; city: string; address: string; description: string;
  stars: number; photo_url: string; version?: number;
}

export type RoomTypeStatus = 'active' | 'archived';
export interface RoomType {
  id: string; hotel_id: string; name: string; description: string;
  capacity: number; total_rooms: number; price: Money; amenities: string[];
  photo_url: string; status: RoomTypeStatus; version: number;
}
export interface RoomTypeInput {
  name: string; description: string; capacity: number; total_rooms: number;
  price: Money; amenities: string[]; photo_url: string; version?: number;
}

export interface Availability {
  room_type_id: string; name: string; capacity: number; total_rooms: number;
  free: number; price_per_night: Money; total_price: Money;
}

export type ReservationStatus = 'confirmed' | 'cancelled';
export interface Reservation {
  id: string; hotel_id: string; hotel_name: string;
  room_type_id: string; room_type_name: string; customer_id: string;
  check_in: string; check_out: string; nights: number; guests: number;
  status: ReservationStatus; total: Money; created_at: string;
  cancelled_at?: string; cancel_reason?: string;
  cancelled_by_role?: Role | 'system'; // только в ответах booking-query
  version: number;
}
export interface BookRequest { room_type_id: string; check_in: string; check_out: string; guests: number }

export interface CustomerSummary {
  upcoming: number;     // подтверждённые, заезд позже сегодняшнего дня
  in_progress: number;  // гость сейчас проживает
  completed: number;    // проживание завершено
  cancelled: number;
  total_spent: Money;   // сумма подтверждённых броней
  next_check_in: string | null;
}
export interface HotelStats {
  hotel_id: string; hotel_name: string; confirmed: number; cancelled: number;
  nights: number; revenue: Money;
}
export interface HotelSummary {
  confirmed: number; cancelled: number;
  upcoming: number;     // подтверждённые с заездом сегодня или позже
  nights: number; guests: number; revenue: Money;
  by_hotel: HotelStats[]; // по убыванию выручки
}

export interface AuditRecord {
  id: number; hotel_id: string; actor_id: string | null; actor_role: Role | 'system';
  action: string; entity_id: string; details: Record<string, unknown>; at: string;
}
export interface Notification {
  id: string; kind: 'reservation_confirmed' | 'reservation_cancelled';
  title: string; body: string; reservation_id?: string; created_at: string; read_at?: string;
}
```

## Экраны и запросы

| Экран | Роль | Запросы |
|---|---|---|
| Поиск отелей | все | `GET /cities`, `GET /hotels?...` |
| Страница отеля и бронирование | все / customer | `GET /hotels/{id}`, `GET /availability`, `POST /reservations` |
| Личный кабинет: мои брони и история | customer | `GET /reservations/my/summary`, `GET /reservations/my?status=...`, `POST /reservations/{id}/cancel` |
| Уведомления | вошедший | `GET /notifications`, `GET /notifications/unread-count` |
| Профиль | вошедший | `GET/PATCH /users/me`, `POST /users/me/password` |
| Мои отели | manager | `GET /hotels/managed`, `PUT /hotels/{id}`, `POST /hotels/{id}/room-types`, `PUT /room-types/{id}` |
| Брони отеля и сводка | manager, admin | `GET /reservations?hotel_id=`, `GET /reservations/summary`, `POST /reservations/{id}/cancel` |
| Аудит отеля | manager, admin | `GET /audit?hotel_id=` |
| Добавление отелей и менеджеров | admin | `POST /hotels`, `PATCH /hotels/{id}/status`, `POST/DELETE /hotels/{id}/managers` |
| Пользователи и роли | admin | `GET /users`, `PATCH /users/{id}/role` |

Вход для разработки: `admin@tourflow.local / Admin12345`. После `./scripts/seed.sh` также доступны `manager@tourflow.local / Manager123` и `client@tourflow.local / Client123`.

### CORS

В основной схеме SPA и API работают на одном origin: dev-сервер Vite проксирует `/api` на `http://localhost:8080`. Для этого в `vite.config.ts` добавьте `server.proxy['/api'] = 'http://localhost:8080'`. Если фронтенд нужно открыть с другого адреса, добавьте этот адрес в `CORS_ORIGINS` у gateway (список через запятую).
