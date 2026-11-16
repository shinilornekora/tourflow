import { useMemo, useState } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  Stack,
  Typography,
} from '@mui/material';
import CheckCircleOutlineIcon from '@mui/icons-material/CheckCircleOutline';
import { useBookMutation } from '@/shared/api/endpoints';
import type { Availability, Hotel, Reservation } from '@/shared/api/types';
import { formatDate, formatMoney, parseError, plural } from '@/shared/lib/format';
import { newKey } from '@/shared/lib/id';

interface Props {
  open: boolean;
  onClose: () => void;
  hotel: Hotel;
  offer: Availability;
  checkIn: string;
  checkOut: string;
  guests: number;
}

/**
 * Подтверждение брони. Ключ идемпотентности создаётся один раз на открытие
 * диалога: двойной клик, повтор после обрыва сети или ретрай — это один и
 * тот же ключ, и сервер вернёт ту же бронь, а не создаст вторую.
 */
export function BookingDialog({ open, onClose, hotel, offer, checkIn, checkOut, guests }: Props) {
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const key = useMemo(() => newKey(), [open, offer.room_type_id, checkIn, checkOut, guests]);
  const [book, { isLoading, error, reset }] = useBookMutation();
  const [done, setDone] = useState<Reservation | null>(null);
  const e = parseError(error);
  const nights = Math.round((Date.parse(checkOut) - Date.parse(checkIn)) / 86_400_000);

  const close = () => {
    setDone(null);
    reset();
    onClose();
  };

  const submit = async () => {
    try {
      const r = await book({
        key,
        room_type_id: offer.room_type_id,
        check_in: checkIn,
        check_out: checkOut,
        guests,
      }).unwrap();
      setDone(r);
    } catch {
      /* ошибка в состоянии мутации */
    }
  };

  return (
    <Dialog open={open} onClose={isLoading ? undefined : close} maxWidth="sm" fullWidth>
      {done ? (
        <>
          <DialogContent sx={{ textAlign: 'center', pt: 5 }}>
            <CheckCircleOutlineIcon color="success" sx={{ fontSize: 64 }} />
            <Typography variant="h3" sx={{ mt: 1 }}>
              Номер забронирован
            </Typography>
            <Typography color="text.secondary" sx={{ mt: 1 }}>
              {hotel.name}, {offer.name}: {formatDate(done.check_in)} — {formatDate(done.check_out)}
            </Typography>
          </DialogContent>
          <DialogActions sx={{ justifyContent: 'center', pb: 4 }}>
            <Button onClick={close}>Остаться здесь</Button>
            <Button variant="contained" component={RouterLink} to="/my/bookings">
              К моим броням
            </Button>
          </DialogActions>
        </>
      ) : (
        <>
          <DialogTitle>Подтвердите бронирование</DialogTitle>
          <DialogContent>
            <Stack spacing={1.5}>
              <Row label="Отель" value={hotel.name} />
              <Row label="Номер" value={offer.name} />
              <Row label="Заезд" value={formatDate(checkIn)} />
              <Row label="Выезд" value={formatDate(checkOut)} />
              <Row label="Гостей" value={String(guests)} />
              <Divider />
              <Row
                label={`${nights} ${plural(nights, 'ночь', 'ночи', 'ночей')} × ${formatMoney(offer.price_per_night)}`}
                value={<b>{formatMoney(offer.total_price)}</b>}
              />
              <Typography variant="body2" color="text.secondary">
                Оплата при заселении. Отменить бронь можно бесплатно до дня заезда.
              </Typography>
              {e && (
                <Alert severity={e.code === 'no_availability' ? 'warning' : 'error'}>
                  {e.code === 'no_availability'
                    ? `${e.message}. Похоже, номер только что забрали — выберите другие даты.`
                    : e.message}
                </Alert>
              )}
            </Stack>
          </DialogContent>
          <DialogActions sx={{ px: 3, pb: 2 }}>
            <Button onClick={close} disabled={isLoading}>
              Отмена
            </Button>
            <Button
              variant="contained"
              onClick={submit}
              disabled={isLoading || e?.code === 'no_availability'}
            >
              {isLoading ? 'Бронируем…' : 'Забронировать'}
            </Button>
          </DialogActions>
        </>
      )}
    </Dialog>
  );
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <Stack direction="row" justifyContent="space-between" spacing={2}>
      <Typography color="text.secondary">{label}</Typography>
      <Typography textAlign="right">{value}</Typography>
    </Stack>
  );
}
