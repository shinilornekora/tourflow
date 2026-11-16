import { useState } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { Alert, Button, Skeleton, Snackbar, Stack, Tab, Tabs } from '@mui/material';
import { useCancelReservationMutation, useMyReservationsQuery } from '@/shared/api/endpoints';
import type { Reservation } from '@/shared/api/types';
import { formatDate, isoDay, parseError } from '@/shared/lib/format';
import { ConfirmDialog, EmptyState, ErrorAlert, PageHeader, Pager } from '@/shared/ui/common';
import { ReservationCard } from '@/entities/reservation/ReservationCard';

type TabKey = 'upcoming' | 'all' | 'cancelled';
const PAGE = 10;

// «Корзина» клиента: текущие брони, полная история и отменённые.
export default function MyBookingsPage() {
  const [tab, setTab] = useState<TabKey>('upcoming');
  const [page, setPage] = useState(1);
  const [toCancel, setToCancel] = useState<Reservation | null>(null);
  const [toast, setToast] = useState('');
  const query =
    tab === 'upcoming'
      ? { status: 'confirmed' as const, from: isoDay(0) }
      : tab === 'cancelled'
        ? { status: 'cancelled' as const }
        : {};
  const { data, isLoading, error, refetch } = useMyReservationsQuery({ ...query, page, page_size: PAGE });
  const [cancel, cancelState] = useCancelReservationMutation();

  const doCancel = async () => {
    if (!toCancel) return;
    try {
      await cancel({ id: toCancel.id }).unwrap();
      setToast('Бронь отменена, номер вернулся в продажу');
      setToCancel(null);
    } catch {
      /* ошибка в диалоге */
    }
  };

  return (
    <>
      <PageHeader title="Мои брони" subtitle="Поездки, история и отменённые бронирования" />
      <Tabs
        value={tab}
        onChange={(_, v: TabKey) => {
          setTab(v);
          setPage(1);
        }}
        sx={{ mb: 3, borderBottom: 1, borderColor: 'divider' }}
      >
        <Tab value="upcoming" label="Предстоящие" />
        <Tab value="all" label="Вся история" />
        <Tab value="cancelled" label="Отменённые" />
      </Tabs>

      <ErrorAlert error={error} onRetry={refetch} />
      {isLoading && <Skeleton variant="rounded" height={140} />}
      {data && data.total === 0 && (
        <EmptyState
          title={tab === 'cancelled' ? 'Отменённых броней нет' : 'Броней пока нет'}
          text="Выберите отель, даты — и номер закрепится за вами сразу."
          action={
            <Button variant="contained" component={RouterLink} to="/">
              Найти отель
            </Button>
          }
        />
      )}

      <Stack spacing={2}>
        {data?.items.map((r) => {
          const cancellable = r.status === 'confirmed' && r.check_in > isoDay(0);
          return (
            <ReservationCard
              key={r.id}
              r={r}
              actions={
                cancellable ? (
                  <Button color="error" variant="outlined" onClick={() => setToCancel(r)}>
                    Отменить
                  </Button>
                ) : null
              }
            />
          );
        })}
      </Stack>
      {data && <Pager page={page} total={data.total} pageSize={PAGE} onChange={setPage} />}

      <ConfirmDialog
        open={Boolean(toCancel)}
        title="Отменить бронь?"
        text={
          toCancel &&
          `${toCancel.hotel_name}, ${formatDate(toCancel.check_in)} — ${formatDate(toCancel.check_out)}. Номер вернётся в продажу, восстановить бронь будет нельзя.`
        }
        confirmText="Отменить бронь"
        danger
        loading={cancelState.isLoading}
        onConfirm={doCancel}
        onClose={() => {
          setToCancel(null);
          cancelState.reset();
        }}
      >
        {cancelState.error && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {parseError(cancelState.error)?.message}
          </Alert>
        )}
      </ConfirmDialog>
      <Snackbar open={Boolean(toast)} autoHideDuration={4000} onClose={() => setToast('')} message={toast} />
    </>
  );
}
