import { useState } from 'react';
import { useParams } from 'react-router-dom';
import {
  Box,
  Card,
  Chip,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import { useAuditQuery, useHotelQuery } from '@/shared/api/endpoints';
import { formatDateTime, roleLabel } from '@/shared/lib/format';
import type { Role } from '@/shared/api/types';
import { EmptyState, ErrorAlert, PageHeader, Pager } from '@/shared/ui/common';

const actionLabel: Record<string, string> = {
  'reservation.created': 'Бронь создана',
  'reservation.cancelled': 'Бронь отменена',
};

// Журнал аудита только для чтения: в базе он append-only (триггер
// запрещает UPDATE/DELETE/TRUNCATE), а RLS показывает записи только
// менеджерам этого отеля и администраторам.
export default function AuditPage() {
  const { id = '' } = useParams();
  const [page, setPage] = useState(1);
  const hotel = useHotelQuery(id);
  const { data, isLoading, error, refetch } = useAuditQuery({ hotel_id: id, page });

  return (
    <>
      <PageHeader title="Журнал аудита" subtitle={hotel.data?.hotel.name} />
      <ErrorAlert error={error} onRetry={refetch} />
      {isLoading && <Skeleton variant="rounded" height={240} />}
      {data?.total === 0 && (
        <EmptyState title="Записей пока нет" text="Здесь появятся все создания и отмены броней." />
      )}
      {data && data.total > 0 && (
        <Card>
          <Box sx={{ overflowX: 'auto' }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Время</TableCell>
                  <TableCell>Действие</TableCell>
                  <TableCell>Кто</TableCell>
                  <TableCell>Бронь</TableCell>
                  <TableCell>Детали</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {data.items.map((a) => (
                  <TableRow key={a.id} hover>
                    <TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDateTime(a.at)}</TableCell>
                    <TableCell>
                      <Chip
                        size="small"
                        color={a.action.endsWith('cancelled') ? 'default' : 'success'}
                        label={actionLabel[a.action] ?? a.action}
                      />
                    </TableCell>
                    <TableCell>
                      {a.actor_role === 'system'
                        ? 'Система'
                        : (roleLabel[a.actor_role as Role] ?? a.actor_role)}
                    </TableCell>
                    <TableCell sx={{ fontFamily: 'monospace' }}>
                      {a.entity_id.slice(0, 8).toUpperCase()}
                    </TableCell>
                    <TableCell>
                      <Typography variant="body2" color="text.secondary">
                        {Object.entries(a.details)
                          .map(([k, v]) => `${k}: ${String(v)}`)
                          .join(' · ')}
                      </Typography>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Box>
        </Card>
      )}
      {data && <Pager page={page} total={data.total} pageSize={data.page_size} onChange={setPage} />}
    </>
  );
}
