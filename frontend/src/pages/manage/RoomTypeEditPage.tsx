import { useEffect, useState, type FormEvent } from 'react';
import { useNavigate, useParams, useMatch } from 'react-router-dom';
import {
  Alert,
  Autocomplete,
  Button,
  Card,
  CardContent,
  Chip,
  InputAdornment,
  Skeleton,
  Stack,
  TextField,
} from '@mui/material';
import {
  useCreateRoomTypeMutation,
  useRoomTypeQuery,
  useUpdateRoomTypeMutation,
} from '@/shared/api/endpoints';
import { parseError } from '@/shared/lib/format';
import { ErrorAlert, PageHeader } from '@/shared/ui/common';

const suggestions = [
  'Wi-Fi',
  'Кондиционер',
  'Завтрак',
  'Мини-бар',
  'Балкон',
  'Вид на море',
  'Сейф',
  'Фен',
  'Ванна',
  'Рабочее место',
];

export default function RoomTypeEditPage() {
  const { id = '' } = useParams();
  const isNew = Boolean(useMatch('/manage/hotels/:id/room-types/new'));
  const navigate = useNavigate();
  const { data, isLoading, error } = useRoomTypeQuery(id, { skip: isNew });
  const [create, cs] = useCreateRoomTypeMutation();
  const [update, us] = useUpdateRoomTypeMutation();
  const [form, setForm] = useState({
    name: '',
    description: '',
    capacity: 2,
    total_rooms: 5,
    price_rub: '4500',
    photo_url: '',
  });
  const [amenities, setAmenities] = useState<string[]>(['Wi-Fi']);

  useEffect(() => {
    if (data) {
      const r = data.room_type;
      setForm({
        name: r.name,
        description: r.description,
        capacity: r.capacity,
        total_rooms: r.total_rooms,
        price_rub: String(r.price.amount / 100),
        photo_url: r.photo_url,
      });
      setAmenities(r.amenities);
    }
  }, [data]);

  if (!isNew && isLoading) return <Skeleton variant="rounded" height={360} />;
  if (!isNew && !data) return <ErrorAlert error={error} title="Категория не найдена" />;

  const hotelId = isNew ? id : data!.room_type.hotel_id;
  const err = parseError(cs.error ?? us.error);
  const fe = (k: string) => err?.fields[k];
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [k]: e.target.value });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    // Рубли → копейки через строку, без float-ошибок (0.1 + 0.2).
    const [rub = '0', kop = ''] = form.price_rub.replace(',', '.').split('.');
    const amount = Number(rub) * 100 + Number((kop + '00').slice(0, 2));
    const body = {
      name: form.name,
      description: form.description,
      capacity: Number(form.capacity),
      total_rooms: Number(form.total_rooms),
      price: { amount, currency: 'RUB' },
      amenities,
      photo_url: form.photo_url,
    };
    try {
      if (isNew) await create({ hotelId, body }).unwrap();
      else await update({ id, hotelId, body: { ...body, version: data!.room_type.version } }).unwrap();
      navigate(`/manage/hotels/${hotelId}`);
    } catch {
      /* ниже */
    }
  };

  return (
    <>
      <PageHeader
        title={isNew ? 'Новая категория номеров' : `Категория «${data!.room_type.name}»`}
        subtitle={data ? data.hotel.name : 'Категория — это тип номера и количество таких номеров в отеле'}
      />
      <Card sx={{ maxWidth: 760 }}>
        <CardContent component="form" onSubmit={submit} sx={{ p: 3 }}>
          <Stack spacing={2}>
            {err && !Object.keys(err.fields).length && <Alert severity="error">{err.message}</Alert>}
            <TextField
              label="Название"
              value={form.name}
              onChange={set('name')}
              error={!!fe('name')}
              helperText={fe('name') ?? 'Например, «Стандарт с видом на реку»'}
              required
            />
            <TextField
              label="Описание"
              value={form.description}
              onChange={set('description')}
              multiline
              minRows={3}
            />
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
              <TextField
                label="Вместимость, гостей"
                type="number"
                inputProps={{ min: 1, max: 10 }}
                value={form.capacity}
                onChange={set('capacity')}
                error={!!fe('capacity')}
                helperText={fe('capacity')}
              />
              <TextField
                label="Количество номеров"
                type="number"
                inputProps={{ min: 1, max: 1000 }}
                value={form.total_rooms}
                onChange={set('total_rooms')}
                error={!!fe('total_rooms')}
                helperText={
                  fe('total_rooms') ?? (isNew ? undefined : 'Уменьшение не отменяет уже проданные брони')
                }
              />
              <TextField
                label="Цена за ночь"
                value={form.price_rub}
                onChange={set('price_rub')}
                inputMode="decimal"
                InputProps={{ endAdornment: <InputAdornment position="end">₽</InputAdornment> }}
                error={!!fe('price.amount')}
                helperText={fe('price.amount')}
              />
            </Stack>
            <Autocomplete
              multiple
              freeSolo
              options={suggestions}
              value={amenities}
              onChange={(_, v) => setAmenities(v)}
              renderTags={(v, getTagProps) =>
                v.map((o, i) => <Chip {...getTagProps({ index: i })} key={o} label={o} size="small" />)
              }
              renderInput={(p) => (
                <TextField
                  {...p}
                  label="Удобства"
                  helperText={fe('amenities') ?? 'Выберите или впишите своё, Enter — добавить'}
                  error={!!fe('amenities')}
                />
              )}
            />
            <TextField
              label="Ссылка на фото"
              value={form.photo_url}
              onChange={set('photo_url')}
              error={!!fe('photo_url')}
              helperText={fe('photo_url') ?? 'Необязательно'}
            />
            <Stack direction="row" spacing={1}>
              <Button type="submit" variant="contained" disabled={cs.isLoading || us.isLoading}>
                {isNew ? 'Создать' : 'Сохранить'}
              </Button>
              <Button onClick={() => navigate(`/manage/hotels/${hotelId}`)}>Отмена</Button>
            </Stack>
          </Stack>
        </CardContent>
      </Card>
    </>
  );
}
