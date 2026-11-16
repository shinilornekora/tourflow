import { Link as RouterLink } from 'react-router-dom';
import { Box, Card, CardActionArea, CardContent, Chip, Rating, Stack, Typography } from '@mui/material';
import PlaceOutlinedIcon from '@mui/icons-material/PlaceOutlined';
import type { Hotel } from '@/shared/api/types';
import { formatMoney } from '@/shared/lib/format';
import { HotelCover } from '@/shared/ui/HotelCover';

export function HotelCard({ hotel, to, search }: { hotel: Hotel; to?: string; search?: string }) {
  return (
    <Card
      sx={{
        height: '100%',
        transition: 'transform .15s, box-shadow .15s',
        '&:hover': { transform: 'translateY(-2px)', boxShadow: 4 },
      }}
    >
      <CardActionArea
        component={RouterLink}
        to={(to ?? `/hotels/${hotel.id}`) + (search ?? '')}
        sx={{ height: '100%', display: 'flex', flexDirection: 'column', alignItems: 'stretch' }}
      >
        <Box sx={{ position: 'relative' }}>
          <HotelCover seed={hotel.id} photo={hotel.photo_url} alt={`Отель ${hotel.name}`} />
          {hotel.status === 'inactive' && (
            <Chip
              label="Не принимает гостей"
              color="error"
              size="small"
              sx={{ position: 'absolute', top: 12, left: 12 }}
            />
          )}
        </Box>
        <CardContent sx={{ flexGrow: 1, display: 'flex', flexDirection: 'column' }}>
          <Stack direction="row" justifyContent="space-between" alignItems="flex-start" spacing={1}>
            <Typography variant="h4" component="h3" sx={{ fontSize: '1.2rem' }}>
              {hotel.name}
            </Typography>
            <Rating value={hotel.stars} readOnly size="small" aria-label={`${hotel.stars} звёзд`} />
          </Stack>
          <Stack direction="row" alignItems="center" spacing={0.5} sx={{ color: 'text.secondary', mt: 0.5 }}>
            <PlaceOutlinedIcon fontSize="small" />
            <Typography variant="body2">{[hotel.city, hotel.address].filter(Boolean).join(', ')}</Typography>
          </Stack>
          <Box sx={{ flexGrow: 1 }} />
          <Stack direction="row" justifyContent="space-between" alignItems="baseline" sx={{ mt: 2 }}>
            <Typography variant="body2" color="text.secondary">
              {hotel.room_type_count ? `${hotel.room_type_count} категор.` : 'Нет номеров'}
            </Typography>
            {hotel.min_price && (
              <Typography>
                <Typography component="span" variant="body2" color="text.secondary">
                  от{' '}
                </Typography>
                <b>{formatMoney(hotel.min_price)}</b>
                <Typography component="span" variant="body2" color="text.secondary">
                  {' '}
                  / ночь
                </Typography>
              </Typography>
            )}
          </Stack>
        </CardContent>
      </CardActionArea>
    </Card>
  );
}
