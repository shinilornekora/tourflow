import { useState } from 'react';
import { Link as RouterLink, useLocation, useNavigate, useParams, useSearchParams } from 'react-router-dom';
import {
  Alert,
  Box,
  Breadcrumbs,
  Button,
  Card,
  CardContent,
  Chip,
  Grid,
  Link,
  MenuItem,
  Paper,
  Rating,
  Skeleton,
  Stack,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import PeopleOutlineIcon from '@mui/icons-material/PeopleOutline';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import { useAppSelector } from '@/app/store';
import { useAvailabilityQuery, useHotelQuery } from '@/shared/api/endpoints';
import type { Availability, RoomType } from '@/shared/api/types';
import { formatMoney, isoDay, nightsBetween, parseError, plural } from '@/shared/lib/format';
import { ErrorAlert } from '@/shared/ui/common';
import { HotelCover } from '@/shared/ui/HotelCover';
import { BookingDialog } from '@/features/booking/BookingDialog';
import NotFoundPage from './NotFoundPage';

export default function HotelPage() {
  const { id = '' } = useParams();
  const [params, setParams] = useSearchParams();
  const user = useAppSelector((s) => s.auth.user);
  const navigate = useNavigate();
  const location = useLocation();

  const checkIn = params.get('check_in') || isoDay(1);
  const checkOut = params.get('check_out') || isoDay(3);
  const guests = Number(params.get('guests')) || 2;
  const nights = nightsBetween(checkIn, checkOut);
  const datesValid = nights > 0 && nights <= 30 && checkIn >= isoDay(0);

  const setParam = (patch: Record<string, string>) => {
    const next = new URLSearchParams(params);
    Object.entries(patch).forEach(([k, v]) => next.set(k, v));
    setParams(next, { replace: true });
  };

  const { data, isLoading, error } = useHotelQuery(id);
  const avail = useAvailabilityQuery(
    { hotel_id: id, check_in: checkIn, check_out: checkOut, guests },
    { skip: !datesValid || !data },
  );
  const [selected, setSelected] = useState<Availability | null>(null);

  if (isLoading) return <Skeleton variant="rounded" height={420} />;
  if (parseError(error)?.status === 404) return <NotFoundPage />;
  if (error || !data) return <ErrorAlert error={error} title="Не удалось загрузить отель" />;

  const { hotel, room_types } = data;
  const canEdit =
    user &&
    (user.role === 'admin' || (user.role === 'hotel_manager' && hotel.manager_ids?.includes(user.id)));
  const offers = new Map((avail.data?.items ?? []).map((a) => [a.room_type_id, a]));
  const isStaff = user?.role === 'admin' || user?.role === 'hotel_manager';
  // Клиент видит только категории, свободные на выбранные даты; персонал — все.
  const visible: RoomType[] = room_types.filter(
    (r) => r.status === 'active' && (isStaff || offers.has(r.id)) && r.capacity >= guests,
  );

  const onBook = (offer: Availability) => {
    if (!user) {
      navigate('/login', { state: { from: location.pathname + location.search } });
      return;
    }
    setSelected(offer);
  };

  return (
    <>
      <Breadcrumbs sx={{ mb: 2 }}>
        <Link component={RouterLink} to="/" underline="hover" color="inherit">
          Отели
        </Link>
        <Typography color="text.primary">{hotel.name}</Typography>
      </Breadcrumbs>

      <Card sx={{ overflow: 'hidden', mb: 4 }}>
        <HotelCover seed={hotel.id} photo={hotel.photo_url} height={280} alt={`Отель ${hotel.name}`} />
        <CardContent sx={{ p: { xs: 2.5, md: 4 } }}>
          <Stack direction={{ xs: 'column', md: 'row' }} justifyContent="space-between" spacing={2}>
            <Box>
              <Stack direction="row" spacing={1.5} alignItems="center" flexWrap="wrap">
                <Typography variant="h2" component="h1">
                  {hotel.name}
                </Typography>
                <Rating value={hotel.stars} readOnly />
                {hotel.status === 'inactive' && <Chip color="error" label="Не принимает гостей" />}
              </Stack>
              <Typography color="text.secondary" sx={{ mt: 0.5 }}>
                {[hotel.city, hotel.address].filter(Boolean).join(', ')}
              </Typography>
            </Box>
            {canEdit && (
              <Button
                variant="outlined"
                startIcon={<EditOutlinedIcon />}
                component={RouterLink}
                to={`/manage/hotels/${hotel.id}`}
              >
                Управление отелем
              </Button>
            )}
          </Stack>
          {hotel.description && (
            <Typography sx={{ mt: 2, whiteSpace: 'pre-line', maxWidth: 820 }}>{hotel.description}</Typography>
          )}
        </CardContent>
      </Card>

      <Paper variant="outlined" sx={{ p: 2, mb: 3, position: { md: 'sticky' }, top: { md: 72 }, zIndex: 2 }}>
        <Grid container spacing={2} alignItems="center">
          <Grid item xs={6} md={3}>
            <TextField
              label="Заезд"
              type="date"
              value={checkIn}
              inputProps={{ min: isoDay(0) }}
              InputLabelProps={{ shrink: true }}
              onChange={(e) => setParam({ check_in: e.target.value })}
            />
          </Grid>
          <Grid item xs={6} md={3}>
            <TextField
              label="Выезд"
              type="date"
              value={checkOut}
              inputProps={{ min: checkIn }}
              InputLabelProps={{ shrink: true }}
              onChange={(e) => setParam({ check_out: e.target.value })}
            />
          </Grid>
          <Grid item xs={6} md={2}>
            <TextField
              select
              label="Гостей"
              value={String(guests)}
              onChange={(e) => setParam({ guests: e.target.value })}
            >
              {[1, 2, 3, 4, 5, 6].map((g) => (
                <MenuItem key={g} value={String(g)}>
                  {g}
                </MenuItem>
              ))}
            </TextField>
          </Grid>
          <Grid item xs={6} md={4}>
            <Typography color="text.secondary">
              {datesValid
                ? `${nights} ${plural(nights, 'ночь', 'ночи', 'ночей')}`
                : 'Выберите даты: от 1 до 30 ночей, не в прошлом'}
            </Typography>
          </Grid>
        </Grid>
      </Paper>

      {isStaff && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Вы видите все категории со счётчиком свободных номеров. Бронировать может только клиент.
        </Alert>
      )}
      {datesValid && (
        <ErrorAlert
          error={avail.error}
          title="Не удалось получить свободные номера"
          onRetry={avail.refetch}
        />
      )}

      <Typography variant="h3" component="h2" sx={{ mb: 2 }}>
        Номера
      </Typography>

      {avail.isFetching && !avail.data && <Skeleton variant="rounded" height={160} />}

      {datesValid && avail.data && visible.length === 0 && (
        <Alert severity="warning">
          На эти даты свободных номеров для {guests} {plural(guests, 'гостя', 'гостей', 'гостей')} нет.
          Попробуйте сдвинуть даты.
        </Alert>
      )}

      <Stack spacing={2}>
        {visible.map((r) => {
          const offer = offers.get(r.id);
          const free = offer?.free ?? 0;
          return (
            <Card key={r.id}>
              <Grid container>
                <Grid item xs={12} md={3}>
                  <HotelCover seed={r.id} photo={r.photo_url} height={170} alt={r.name} />
                </Grid>
                <Grid item xs={12} md={9}>
                  <CardContent>
                    <Stack direction={{ xs: 'column', sm: 'row' }} justifyContent="space-between" spacing={2}>
                      <Box>
                        <Typography variant="h4" component="h3">
                          {r.name}
                        </Typography>
                        <Stack
                          direction="row"
                          spacing={1}
                          alignItems="center"
                          sx={{ color: 'text.secondary', mt: 0.5 }}
                        >
                          <PeopleOutlineIcon fontSize="small" />
                          <Typography variant="body2">до {r.capacity} гостей</Typography>
                          {isStaff && offer && (
                            <Chip
                              size="small"
                              color={free ? 'success' : 'default'}
                              label={`свободно ${free} из ${r.total_rooms}`}
                            />
                          )}
                        </Stack>
                        {r.description && (
                          <Typography variant="body2" sx={{ mt: 1, maxWidth: 560 }}>
                            {r.description}
                          </Typography>
                        )}
                        <Stack direction="row" spacing={0.75} flexWrap="wrap" useFlexGap sx={{ mt: 1.5 }}>
                          {r.amenities.map((a) => (
                            <Chip key={a} label={a} size="small" variant="outlined" />
                          ))}
                        </Stack>
                      </Box>
                      <Stack
                        alignItems={{ xs: 'flex-start', sm: 'flex-end' }}
                        spacing={1}
                        sx={{ minWidth: 180 }}
                      >
                        <Typography variant="body2" color="text.secondary">
                          {formatMoney(r.price)} / ночь
                        </Typography>
                        {offer && (
                          <Typography variant="h4" component="p">
                            {formatMoney(offer.total_price)}
                          </Typography>
                        )}
                        <Tooltip title={isStaff ? 'Бронировать может только клиент' : ''}>
                          <span>
                            <Button
                              variant="contained"
                              disabled={
                                !offer || free === 0 || isStaff || !datesValid || hotel.status !== 'active'
                              }
                              onClick={() => offer && onBook(offer)}
                            >
                              {user ? 'Забронировать' : 'Войти и забронировать'}
                            </Button>
                          </span>
                        </Tooltip>
                      </Stack>
                    </Stack>
                  </CardContent>
                </Grid>
              </Grid>
            </Card>
          );
        })}
      </Stack>

      {selected && (
        <BookingDialog
          open={Boolean(selected)}
          onClose={() => setSelected(null)}
          hotel={hotel}
          offer={selected}
          checkIn={checkIn}
          checkOut={checkOut}
          guests={guests}
        />
      )}
    </>
  );
}
