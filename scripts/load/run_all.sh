#!/bin/bash
# Паспорт нагрузки (docs/LOAD.md): все 18 прогонов подряд, вывод каждой серии —
# в свой файл. Запускать на заново созданных базах после `loadgen -prepare` и
# заливки scripts/load/fill.sql. Ничего не удаляет и не повторяет «неудачные»
# прогоны: что получилось, то и приложено.
#
#   LOADGEN=./loadgen STATE=state.json OUT=. scripts/load/run_all.sh
set -u
L=${LOADGEN:-./loadgen}; S=${STATE:-state.json}; O=${OUT:-.}
for w in 1 8 32; do
  if [ "$w" = 32 ]; then
    # Загрузка процессора во время трёх прогонов shop при 32 воркерах.
    (vmstat 5 19 > "$O/cpu_vmstat.txt" &)
    (top -b -d 10 -n 9 -w 200 | grep -E "^top|^%Cpu|postgres|booking|gateway|loadgen|notification|java|catalog|identity" > "$O/cpu_top_raw.txt" &)
  fi
  "$L" -state "$S" -mix shop -workers "$w" -duration 30s -runs 3 > "$O/shop_w$w.txt" 2>&1
done
i=0
for w in 1 8 32; do
  # У каждого прогона hot своё окно в 30 суток внутри залитых данных.
  "$L" -state "$S" -mix hot -workers "$w" -duration 20s -runs 3 -window 30 -window-start $((20 + 90 * i)) > "$O/hot_w$w.txt" 2>&1
  i=$((i + 1))
done
