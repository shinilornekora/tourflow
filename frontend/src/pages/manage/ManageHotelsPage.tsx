import { useState } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { Button, Grid, MenuItem, Skeleton, Stack, TextField } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import { useRole } from '@/features/auth/guards';
import { useManagedHotelsQuery } from '@/shared/api/endpoints';
import { HotelCard } from '@/entities/hotel/HotelCard';
import { EmptyState, ErrorAlert, PageHeader, Pager } from '@/shared/ui/common';
import { useDebounced } from '@/shared/lib/useDebounced';

const PAGE = 9;

export default function ManageHotelsPage() {
  const role = useRole();
  const isAdmin = role === 'admin';
  const [q, setQ] = useState('');
  const [sort, setSort] = useState('name');
  const [page, setPage] = useState(1);
  const dq = useDebounced(q);
  const { data, isLoading, error, refetch } = useManagedHotelsQuery({ q: dq, sort, page, page_size: PAGE });

  return (
    <>
      <PageHeader
        title={isAdmin ? 'Управление отелями' : 'Мои отели'}
        subtitle={
          isAdmin ? 'Все отели системы, включая неактивные' : 'Отели, которые вам назначил администратор'
        }
        actions={
          isAdmin && (
            <Button
              variant="contained"
              startIcon={<AddIcon />}
              component={RouterLink}
              to="/manage/hotels/new"
            >
              Новый отель
            </Button>
          )
        }
      />
      <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} sx={{ mb: 3 }}>
        <TextField
          label="Поиск по названию"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setPage(1);
          }}
        />
        <TextField
          select
          label="Сортировка"
          value={sort}
          onChange={(e) => setSort(e.target.value)}
          sx={{ minWidth: 220 }}
        >
          <MenuItem value="name">По названию</MenuItem>
          <MenuItem value="-created_at">Новые сначала</MenuItem>
          <MenuItem value="-stars">По звёздности</MenuItem>
        </TextField>
      </Stack>
      <ErrorAlert error={error} onRetry={refetch} />
      {isLoading && <Skeleton variant="rounded" height={280} />}
      {data?.total === 0 && (
        <EmptyState
          title={isAdmin ? 'Отелей ещё нет' : 'Вам пока не назначен ни один отель'}
          text={
            isAdmin
              ? 'Создайте первый отель и назначьте ему менеджера.'
              : 'Попросите администратора назначить вас менеджером отеля.'
          }
        />
      )}
      <Grid container spacing={3}>
        {data?.items.map((h) => (
          <Grid item xs={12} sm={6} md={4} key={h.id}>
            <HotelCard hotel={h} to={`/manage/hotels/${h.id}`} />
          </Grid>
        ))}
      </Grid>
      {data && <Pager page={page} total={data.total} pageSize={PAGE} onChange={setPage} />}
    </>
  );
}
