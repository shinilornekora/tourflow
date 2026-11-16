import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import {
  Alert,
  Button,
  Grid,
  MenuItem,
  Paper,
  Skeleton,
  Snackbar,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import {
  useCancelReservationMutation,
  useHotelReservationsQuery,
  useManagedHotelsQuery,
} from '@/shared/api/endpoints';
import type { Reservation } from '@/shared/api/types';
import { formatDate, isoDay, parseError, plural } from '@/shared/lib/format';
import { ConfirmDialog, EmptyState, ErrorAlert, PageHeader, Pager } from '@/shared/ui/common';
import { ReservationCard } from '@/entities/reservation/ReservationCard';

const PAGE = 10;

// Брони отелей для менеджера (только свои отели — это гарантирует RLS
// в booking) и администратора (все отели).
export default function ManageBookingsPage() {
  const [params, setParams] = useSearchParams();
  const hotels = useManagedHotelsQuery({ page_size: 50 });
  const hotelId = params.get('hotel_id') ?? '';
  const status = (params.get('status') ?? '') as '' | 'confirmed' | 'cancelled';
  const from = params.get('from') ?? '';
  const to = params.get('to') ?? '';
  const page = Number(params.get('page')) || 1;
  const set = (patch: Record<string, string>) => {
    const n = new URLSearchParams(params);
    Object.entries(patch).forEach(([k, v]) => (v ? n.set(k, v) : n.delete(k)));
    if (!('page' in patch)) n.delete('page');
    setParams(n, { replace: true });
  };

  useEffect(() => {
    const first = hotels.data?.items[0];
    if (!hotelId && first) set({ hotel_id: first.id });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hotels.data, hotelId]);

  const { data, isLoading, error, refetch } = useHotelReservationsQuery(
    { hotel_id: hotelId, status, from, to, page, page_size: PAGE },
    { skip: !hotelId },
  );
  const [cancel, cs] = useCancelReservationMutation();
  const [target, setTarget] = useState<Reservation | null>(null);
  const [reason, setReason] = useState('');
  const [toast, setToast] = useState('');

  const doCancel = async () => {
    if (!target) return;
    try {
      await cancel({ id: target.id, reason }).unwrap();
      setTarget(null);
      setReason('');
      setToast('Бронь отменена, гость получит уведомление');
    } catch {
      /* в диалоге */
    }
  };

  return (
    <>
      <PageHeader title="Брони отелей" subtitle="Заезды, проживающие и история по каждому отелю" />
      <Paper variant="outlined" sx={{ p: 2, mb: 3 }}>
        <Grid container spacing={2}>
          <Grid item xs={12} md={4}>
            <TextField
              select
              label="Отель"
              value={hotelId}
              onChange={(e) => set({ hotel_id: e.target.value })}
            >
              {(hotels.data?.items ?? []).length === 0 && (
                <MenuItem value={hotelId} disabled>
                  {hotels.isLoading ? 'Загрузка…' : 'Нет отелей'}
                </MenuItem>
              )}
              {hotels.data?.items.map((h) => (
                <MenuItem key={h.id} value={h.id}>
                  {h.name} · {h.city}
                </MenuItem>
              ))}
            </TextField>
          </Grid>
          <Grid item xs={12} sm={4} md={2}>
            <TextField select label="Статус" value={status} onChange={(e) => set({ status: e.target.value })}>
              <MenuItem value="">Все</MenuItem>
              <MenuItem value="confirmed">Подтверждённые</MenuItem>
              <MenuItem value="cancelled">Отменённые</MenuItem>
            </TextField>
          </Grid>
          <Grid item xs={6} sm={4} md={3}>
            <TextField
              label="Проживание с"
              type="date"
              value={from}
              InputLabelProps={{ shrink: true }}
              onChange={(e) => set({ from: e.target.value })}
            />
          </Grid>
          <Grid item xs={6} sm={4} md={3}>
            <TextField
              label="по"
              type="date"
              value={to}
              InputLabelProps={{ shrink: true }}
              onChange={(e) => set({ to: e.target.value })}
            />
          </Grid>
        </Grid>
        <Stack direction="row" spacing={1} sx={{ mt: 2 }}>
          <Button size="small" onClick={() => set({ from: isoDay(0), to: isoDay(1), status: 'confirmed' })}>
            Сегодня в отеле
          </Button>
          <Button size="small" onClick={() => set({ from: isoDay(0), to: isoDay(7), status: 'confirmed' })}>
            Ближайшие 7 дней
          </Button>
          <Button size="small" onClick={() => set({ from: '', to: '', status: '' })}>
            Сбросить
          </Button>
        </Stack>
      </Paper>

      {hotels.data?.total === 0 && <EmptyState title="Нет отелей" text="Вам не назначен ни один отель." />}
      <ErrorAlert error={error} onRetry={refetch} />
      {isLoading && <Skeleton variant="rounded" height={140} />}
      {data && (
        <Typography color="text.secondary" sx={{ mb: 2 }}>
          {data.total} {plural(data.total, 'бронь', 'брони', 'броней')}
        </Typography>
      )}
      {data?.total === 0 && (
        <EmptyState title="Броней нет" text="Под выбранные фильтры ничего не подходит." />
      )}
      <Stack spacing={2}>
        {data?.items.map((r) => (
          <ReservationCard
            key={r.id}
            r={r}
            showCustomer
            actions={
              r.status === 'confirmed' && r.check_out > isoDay(0) ? (
                <Button color="error" variant="outlined" onClick={() => setTarget(r)}>
                  Отменить
                </Button>
              ) : null
            }
          />
        ))}
      </Stack>
      {data && (
        <Pager page={page} total={data.total} pageSize={PAGE} onChange={(p) => set({ page: String(p) })} />
      )}

      <ConfirmDialog
        open={Boolean(target)}
        title="Отменить бронь гостя?"
        text={
          target &&
          `${target.room_type_name}, ${formatDate(target.check_in)} — ${formatDate(target.check_out)}. Гость получит уведомление с причиной.`
        }
        confirmText="Отменить бронь"
        danger
        loading={cs.isLoading}
        onConfirm={doCancel}
        onClose={() => {
          setTarget(null);
          cs.reset();
        }}
      >
        <TextField
          sx={{ mt: 2 }}
          label="Причина для гостя"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          multiline
          minRows={2}
          error={!!parseError(cs.error)?.fields.reason}
          helperText={parseError(cs.error)?.fields.reason}
          autoFocus
        />
        {cs.error && !parseError(cs.error)?.fields.reason && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {parseError(cs.error)?.message}
          </Alert>
        )}
      </ConfirmDialog>
      <Snackbar open={Boolean(toast)} autoHideDuration={4000} onClose={() => setToast('')} message={toast} />
    </>
  );
}
