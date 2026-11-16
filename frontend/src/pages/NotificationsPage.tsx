import { useState } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import {
  Badge,
  Box,
  Button,
  Card,
  CardActionArea,
  CardContent,
  Skeleton,
  Stack,
  Typography,
} from '@mui/material';
import { useMarkAllReadMutation, useMarkReadMutation, useNotificationsQuery } from '@/shared/api/endpoints';
import { formatDateTime } from '@/shared/lib/format';
import { EmptyState, ErrorAlert, PageHeader, Pager } from '@/shared/ui/common';

export default function NotificationsPage() {
  const [page, setPage] = useState(1);
  const { data, isLoading, error, refetch } = useNotificationsQuery({ page });
  const [markRead] = useMarkReadMutation();
  const [markAll, all] = useMarkAllReadMutation();
  const unread = data?.items.some((n) => !n.read_at);

  return (
    <>
      <PageHeader
        title="Уведомления"
        subtitle="Подтверждения и отмены броней"
        actions={
          <Button variant="outlined" onClick={() => markAll()} disabled={!unread || all.isLoading}>
            Прочитать все
          </Button>
        }
      />
      <ErrorAlert error={error} onRetry={refetch} />
      {isLoading && <Skeleton variant="rounded" height={120} />}
      {data?.total === 0 && <EmptyState title="Пока тихо" text="Здесь появятся сообщения о ваших бронях." />}
      <Stack spacing={1.5}>
        {data?.items.map((n) => (
          <Card
            key={n.id}
            sx={{
              borderLeft: 4,
              borderLeftColor: n.kind === 'reservation_cancelled' ? 'error.main' : 'success.main',
            }}
          >
            <CardActionArea
              component={n.reservation_id ? RouterLink : 'div'}
              to={n.reservation_id ? '/my/bookings' : undefined}
              onClick={() => !n.read_at && markRead(n.id)}
            >
              <CardContent>
                <Stack direction="row" justifyContent="space-between" spacing={2}>
                  <Box>
                    <Badge variant="dot" color="secondary" invisible={Boolean(n.read_at)}>
                      <Typography fontWeight={700}>{n.title}</Typography>
                    </Badge>
                    <Typography color="text.secondary" sx={{ mt: 0.5 }}>
                      {n.body}
                    </Typography>
                  </Box>
                  <Typography variant="caption" color="text.secondary" sx={{ whiteSpace: 'nowrap' }}>
                    {formatDateTime(n.created_at)}
                  </Typography>
                </Stack>
              </CardContent>
            </CardActionArea>
          </Card>
        ))}
      </Stack>
      {data && <Pager page={page} total={data.total} pageSize={data.page_size} onChange={setPage} />}
    </>
  );
}
