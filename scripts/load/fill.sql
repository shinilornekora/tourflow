-- Заливка объёма для паспорта нагрузки (РХД ПР-1): ~100 тыс. подтверждённых
-- броней на 340 суток вперёд по отелям из state.json. Число броней на сутки
-- убывает с рангом отеля как 80/ранг (распределение Зипфа: первые отели
-- «горячие»). Остатки заливаются согласованно с бронями — сверка после
-- заливки обязана вернуть ноль расхождений.
--
--   psql "postgres://booking_svc:booking_pw@localhost:5432/booking" \
--        -v rooms="'{id1,id2,…}'" -f scripts/load/fill.sql
BEGIN;
SELECT set_config('app.role', 'system', true);
CREATE TEMP TABLE plan ON COMMIT DROP AS
SELECT r.room_type_id, r.hotel_id, h.name AS hotel_name, r.name AS room_name, r.price_amount,
       d::date AS day, greatest(1, round(80.0 / ord))::int AS k
FROM unnest(:rooms::uuid[]) WITH ORDINALITY AS t(id, ord)
JOIN room_type_replica r ON r.room_type_id = t.id
JOIN hotel_replica h ON h.hotel_id = r.hotel_id
CROSS JOIN generate_series(current_date + 1, current_date + 340, interval '1 day') d;

INSERT INTO reservations(id, hotel_id, room_type_id, customer_id, check_in, check_out, guests, status,
                         total_amount, currency, hotel_name, room_type_name)
SELECT gen_random_uuid(), p.hotel_id, p.room_type_id, gen_random_uuid(), p.day, p.day + 1, 2, 'confirmed',
       p.price_amount, 'RUB', p.hotel_name, p.room_name
FROM plan p, generate_series(1, p.k);

INSERT INTO inventory(room_type_id, day, booked)
SELECT room_type_id, day, k FROM plan
ON CONFLICT (room_type_id, day) DO UPDATE SET booked = inventory.booked + EXCLUDED.booked;
COMMIT;
ANALYZE reservations;
ANALYZE inventory;
