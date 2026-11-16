import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import {
  Box,
  Grid,
  InputAdornment,
  MenuItem,
  Paper,
  Skeleton,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import SearchIcon from '@mui/icons-material/Search';
import { useCitiesQuery, useHotelsQuery } from '@/shared/api/endpoints';
import { HotelCard } from '@/entities/hotel/HotelCard';
import { EmptyState, ErrorAlert, Pager } from '@/shared/ui/common';
import { useDebounced } from '@/shared/lib/useDebounced';
import { plural } from '@/shared/lib/format';

const PAGE_SIZE = 9;

const sorts = [
  { v: 'name', l: 'По названию' },
  { v: 'price', l: 'Сначала дешевле' },
  { v: '-price', l: 'Сначала дороже' },
  { v: '-stars', l: 'По звёздности' },
  { v: '-created_at', l: 'Новые' },
];

// Состояние фильтров живёт в URL: выдачу можно отправить ссылкой,
// кнопка «назад» возвращает к прошлым фильтрам, F5 ничего не сбрасывает.
export default function HotelsPage() {
  const [params, setParams] = useSearchParams();
  const get = (k: string) => params.get(k) ?? '';
  const [q, setQ] = useState(get('q'));
  const debouncedQ = useDebounced(q);

  const update = (patch: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v);
      else next.delete(k);
    }
    if (!('page' in patch)) next.delete('page');
    setParams(next, { replace: true });
  };

  useEffect(() => {
    if (debouncedQ !== get('q')) update({ q: debouncedQ });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debouncedQ]);

  const page = Number(get('page')) || 1;
  const priceMaxRub = Number(get('price_max')) || 0;
  const { data, isFetching, isLoading, error, refetch } = useHotelsQuery({
    q: get('q'),
    city: get('city'),
    stars_min: Number(get('stars')) || 0,
    guests: Number(get('guests')) || 0,
    price_max: priceMaxRub * 100,
    sort: get('sort') || 'name',
    page,
    page_size: PAGE_SIZE,
  });
  const { data: cities } = useCitiesQuery();

  return (
    <>
      <Box
        sx={{
          mb: 4,
          p: { xs: 3, md: 5 },
          borderRadius: 4,
          color: 'primary.contrastText',
          background: 'linear-gradient(120deg, #0a4348 0%, #0f5c63 55%, #3f8a90 100%)',
          position: 'relative',
          overflow: 'hidden',
        }}
      >
        <Box
          sx={{
            position: 'absolute',
            right: { xs: -70, md: 40 },
            top: { xs: -70, md: 36 },
            width: { xs: 140, md: 180 },
            height: { xs: 140, md: 180 },
            borderRadius: '50%',
            bgcolor: 'secondary.main',
            opacity: { xs: 0.35, md: 0.9 },
          }}
        />
        <Typography variant="h1" sx={{ position: 'relative', maxWidth: 640 }}>
          Найдите отель для следующей поездки
        </Typography>
        <Typography sx={{ position: 'relative', mt: 1, opacity: 0.9, maxWidth: 560 }}>
          Подтверждение сразу — номер закрепляется за вами в момент бронирования.
        </Typography>
      </Box>

      <Paper variant="outlined" sx={{ p: 2, mb: 3 }}>
        <Grid container spacing={2} columns={24}>
          <Grid item xs={24} md={7}>
            <TextField
              label="Название отеля"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              InputProps={{
                startAdornment: (
                  <InputAdornment position="start">
                    <SearchIcon />
                  </InputAdornment>
                ),
              }}
            />
          </Grid>
          <Grid item xs={12} md={4}>
            <TextField
              select
              label="Город"
              value={get('city')}
              onChange={(e) => update({ city: e.target.value })}
            >
              <MenuItem value="">Любой</MenuItem>
              {cities?.items.map((c) => (
                <MenuItem key={c} value={c}>
                  {c}
                </MenuItem>
              ))}
            </TextField>
          </Grid>
          <Grid item xs={12} md={3}>
            <TextField
              select
              label="Звёзд от"
              value={get('stars')}
              onChange={(e) => update({ stars: e.target.value })}
            >
              <MenuItem value="">Любая</MenuItem>
              {[2, 3, 4, 5].map((s) => (
                <MenuItem key={s} value={String(s)}>
                  {s}★
                </MenuItem>
              ))}
            </TextField>
          </Grid>
          <Grid item xs={12} md={3}>
            <TextField
              select
              label="Гостей"
              value={get('guests')}
              onChange={(e) => update({ guests: e.target.value })}
            >
              <MenuItem value="">Любое</MenuItem>
              {[1, 2, 3, 4, 5, 6].map((g) => (
                <MenuItem key={g} value={String(g)}>
                  {g}
                </MenuItem>
              ))}
            </TextField>
          </Grid>
          <Grid item xs={12} md={3}>
            <TextField
              label="Цена до, ₽"
              type="number"
              inputProps={{ min: 0, step: 500 }}
              value={get('price_max')}
              onChange={(e) => update({ price_max: e.target.value })}
            />
          </Grid>
          <Grid item xs={24} md={4}>
            <TextField
              select
              label="Сортировка"
              value={get('sort') || 'name'}
              onChange={(e) => update({ sort: e.target.value })}
            >
              {sorts.map((s) => (
                <MenuItem key={s.v} value={s.v}>
                  {s.l}
                </MenuItem>
              ))}
            </TextField>
          </Grid>
        </Grid>
      </Paper>

      <ErrorAlert error={error} title="Не удалось загрузить отели" onRetry={refetch} />

      {data && (
        <Typography color="text.secondary" sx={{ mb: 2 }}>
          Найдено {data.total} {plural(data.total, 'отель', 'отеля', 'отелей')}
        </Typography>
      )}

      <Grid
        container
        spacing={3}
        sx={{ opacity: isFetching && !isLoading ? 0.6 : 1, transition: 'opacity .2s' }}
      >
        {isLoading &&
          Array.from({ length: 6 }, (_, i) => (
            <Grid item xs={12} sm={6} md={4} key={i}>
              <Skeleton variant="rounded" height={300} />
            </Grid>
          ))}
        {data?.items.map((h) => (
          <Grid item xs={12} sm={6} md={4} key={h.id}>
            <HotelCard hotel={h} search={get('guests') ? `?guests=${get('guests')}` : ''} />
          </Grid>
        ))}
      </Grid>

      {data && data.total === 0 && (
        <EmptyState
          title="Ничего не нашлось"
          text="Попробуйте убрать часть фильтров или выбрать другой город."
        />
      )}

      {data && (
        <Stack>
          <Pager
            page={page}
            total={data.total}
            pageSize={PAGE_SIZE}
            onChange={(p) => update({ page: String(p) })}
          />
        </Stack>
      )}
    </>
  );
}
