import type { ReactNode } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { Box, Card, CardContent, Chip, Link, Stack, Typography } from '@mui/material';
import type { Reservation } from '@/shared/api/types';
import { formatDate, formatDateTime, formatMoney, plural, statusLabel } from '@/shared/lib/format';

export function ReservationCard({
  r,
  actions,
  showCustomer,
}: {
  r: Reservation;
  actions?: ReactNode;
  showCustomer?: boolean;
}) {
  const cancelled = r.status === 'cancelled';
  return (
    <Card sx={{ opacity: cancelled ? 0.75 : 1 }}>
      <CardContent>
        <Stack direction={{ xs: 'column', md: 'row' }} justifyContent="space-between" spacing={2}>
          <Box>
            <Stack direction="row" spacing={1} alignItems="center" flexWrap="wrap">
              <Link component={RouterLink} to={`/hotels/${r.hotel_id}`} underline="hover" color="inherit">
                <Typography variant="h4" component="h3">
                  {r.hotel_name}
                </Typography>
              </Link>
              <Chip size="small" color={cancelled ? 'default' : 'success'} label={statusLabel[r.status]} />
            </Stack>
            <Typography color="text.secondary" sx={{ mt: 0.5 }}>
              {r.room_type_name} · {r.guests} {plural(r.guests, 'гость', 'гостя', 'гостей')}
            </Typography>
            <Typography sx={{ mt: 1 }}>
              <b>{formatDate(r.check_in)}</b> — <b>{formatDate(r.check_out)}</b> · {r.nights}{' '}
              {plural(r.nights, 'ночь', 'ночи', 'ночей')}
            </Typography>
            {cancelled && r.cancel_reason && (
              <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
                Причина отмены: {r.cancel_reason}
              </Typography>
            )}
            <Typography variant="caption" color="text.secondary" component="div" sx={{ mt: 1 }}>
              Бронь № {r.id.slice(0, 8).toUpperCase()} · создана {formatDateTime(r.created_at)}
              {showCustomer && ` · гость ${r.customer_id.slice(0, 8)}`}
            </Typography>
          </Box>
          <Stack alignItems={{ xs: 'flex-start', md: 'flex-end' }} justifyContent="space-between" spacing={1}>
            <Typography
              variant="h4"
              component="p"
              sx={{ textDecoration: cancelled ? 'line-through' : 'none' }}
            >
              {formatMoney(r.total)}
            </Typography>
            {actions}
          </Stack>
        </Stack>
      </CardContent>
    </Card>
  );
}
