import { useEffect, useState, type FormEvent } from 'react';
import { Link as RouterLink, useNavigate, useParams } from 'react-router-dom';
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  Grid,
  IconButton,
  Rating,
  Skeleton,
  Snackbar,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Inventory2OutlinedIcon from '@mui/icons-material/Inventory2Outlined';
import UnarchiveOutlinedIcon from '@mui/icons-material/UnarchiveOutlined';
import PersonRemoveOutlinedIcon from '@mui/icons-material/PersonRemoveOutlined';
import { useRole } from '@/features/auth/guards';
import {
  useAssignManagerMutation,
  useCreateHotelMutation,
  useHotelQuery,
  useRemoveManagerMutation,
  useSetHotelStatusMutation,
  useSetRoomStatusMutation,
  useUpdateHotelMutation,
  useUsersQuery,
} from '@/shared/api/endpoints';
import type { HotelInput, RoomType, User } from '@/shared/api/types';
import { formatMoney, parseError } from '@/shared/lib/format';
import { ConfirmDialog, ErrorAlert, PageHeader } from '@/shared/ui/common';

const empty: HotelInput = { name: '', city: '', address: '', description: '', stars: 3, photo_url: '' };

export default function HotelEditPage() {
  const { id } = useParams();
  const isNew = !id;
  const role = useRole();
  const isAdmin = role === 'admin';
  const navigate = useNavigate();
  const { data, isLoading, error } = useHotelQuery(id ?? '', { skip: isNew });
  const [create, createState] = useCreateHotelMutation();
  const [update, updateState] = useUpdateHotelMutation();
  const [setStatus, statusState] = useSetHotelStatusMutation();
  const [form, setForm] = useState<HotelInput>(empty);
  const [confirmStatus, setConfirmStatus] = useState(false);
  const [toast, setToast] = useState('');

  useEffect(() => {
    if (data) {
      const h = data.hotel;
      setForm({
        name: h.name,
        city: h.city,
        address: h.address,
        description: h.description,
        stars: h.stars,
        photo_url: h.photo_url,
      });
    }
  }, [data]);

  if (!isNew && isLoading) return <Skeleton variant="rounded" height={400} />;
  if (!isNew && !data) return <ErrorAlert error={error} title="Отель не найден" />;

  const hotel = data?.hotel;
  const saveErr = parseError(createState.error ?? updateState.error);
  const fe = (k: string) => saveErr?.fields[k];
  const set = (k: keyof HotelInput) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [k]: k === 'stars' ? Number(e.target.value) : e.target.value });

  const onSave = async (e: FormEvent) => {
    e.preventDefault();
    try {
      if (isNew) {
        const h = await create(form).unwrap();
        navigate(`/manage/hotels/${h.id}`, { replace: true });
      } else if (hotel) {
        await update({ id: hotel.id, body: { ...form, version: hotel.version } }).unwrap();
        setToast('Изменения сохранены');
      }
    } catch {
      /* показано ниже */
    }
  };

  const toggleStatus = async () => {
    if (!hotel) return;
    try {
      await setStatus({
        id: hotel.id,
        status: hotel.status === 'active' ? 'inactive' : 'active',
        version: hotel.version,
      }).unwrap();
      setConfirmStatus(false);
      setToast(
        hotel.status === 'active'
          ? 'Отель отключён. Будущие брони гостей отменяются.'
          : 'Отель снова принимает гостей',
      );
    } catch {
      /* в диалоге */
    }
  };

  return (
    <>
      <PageHeader
        title={isNew ? 'Новый отель' : hotel!.name}
        subtitle={
          hotel && (
            <Stack direction="row" spacing={1} alignItems="center" component="span">
              <Chip
                size="small"
                color={hotel.status === 'active' ? 'success' : 'error'}
                label={hotel.status === 'active' ? 'Принимает гостей' : 'Отключён'}
              />
              <span>версия {hotel.version}</span>
            </Stack>
          )
        }
        actions={
          hotel && (
            <>
              <Button component={RouterLink} to={`/hotels/${hotel.id}`}>
                Страница отеля
              </Button>
              <Button component={RouterLink} to={`/manage/bookings?hotel_id=${hotel.id}`}>
                Брони
              </Button>
              <Button component={RouterLink} to={`/manage/hotels/${hotel.id}/audit`}>
                Журнал
              </Button>
            </>
          )
        }
      />

      <Grid container spacing={3}>
        <Grid item xs={12} md={isAdmin && hotel ? 7 : 12}>
          <Card>
            <CardContent component="form" onSubmit={onSave} sx={{ p: 3 }}>
              <Typography variant="h4" sx={{ mb: 2 }}>
                Информация об отеле
              </Typography>
              <Stack spacing={2}>
                {saveErr && !Object.keys(saveErr.fields).length && (
                  <Alert severity={saveErr.code === 'version_mismatch' ? 'warning' : 'error'}>
                    {saveErr.message}
                  </Alert>
                )}
                <TextField
                  label="Название"
                  value={form.name}
                  onChange={set('name')}
                  error={!!fe('name')}
                  helperText={fe('name')}
                  required
                />
                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
                  <TextField
                    label="Город"
                    value={form.city}
                    onChange={set('city')}
                    error={!!fe('city')}
                    helperText={fe('city')}
                    required
                  />
                  <Box sx={{ minWidth: 180 }}>
                    <Typography variant="caption" color="text.secondary">
                      Звёздность
                    </Typography>
                    <Rating value={form.stars} onChange={(_, v) => v && setForm({ ...form, stars: v })} />
                  </Box>
                </Stack>
                <TextField
                  label="Адрес"
                  value={form.address}
                  onChange={set('address')}
                  error={!!fe('address')}
                  helperText={fe('address')}
                />
                <TextField
                  label="Описание"
                  value={form.description}
                  onChange={set('description')}
                  multiline
                  minRows={4}
                  error={!!fe('description')}
                  helperText={fe('description')}
                />
                <TextField
                  label="Ссылка на фото"
                  value={form.photo_url}
                  onChange={set('photo_url')}
                  error={!!fe('photo_url')}
                  helperText={fe('photo_url') ?? 'Необязательно: https://…'}
                />
                <Button
                  type="submit"
                  variant="contained"
                  disabled={createState.isLoading || updateState.isLoading}
                  sx={{ alignSelf: 'flex-start' }}
                >
                  {isNew ? 'Создать отель' : 'Сохранить'}
                </Button>
              </Stack>
            </CardContent>
          </Card>
        </Grid>

        {hotel && isAdmin && (
          <Grid item xs={12} md={5}>
            <Stack spacing={3}>
              {isAdmin && (
                <ManagersCard hotelId={hotel.id} managerIds={hotel.manager_ids ?? []} onToast={setToast} />
              )}
              {isAdmin && (
                <Card>
                  <CardContent sx={{ p: 3 }}>
                    <Typography variant="h4">Статус</Typography>
                    <Typography color="text.secondary" sx={{ mt: 1, mb: 2 }}>
                      {hotel.status === 'active'
                        ? 'Отключение скроет отель из каталога и отменит все брони с заездом позже сегодняшнего дня. Гости получат уведомление.'
                        : 'Отель скрыт из каталога и не продаёт номера.'}
                    </Typography>
                    <Button
                      variant="outlined"
                      color={hotel.status === 'active' ? 'error' : 'success'}
                      onClick={() => setConfirmStatus(true)}
                    >
                      {hotel.status === 'active' ? 'Отключить отель' : 'Включить отель'}
                    </Button>
                  </CardContent>
                </Card>
              )}
            </Stack>
          </Grid>
        )}

        {hotel && data && (
          <Grid item xs={12}>
            <RoomTypesCard hotelId={hotel.id} rooms={data.room_types} onToast={setToast} />
          </Grid>
        )}
      </Grid>

      <ConfirmDialog
        open={confirmStatus}
        title={hotel?.status === 'active' ? 'Отключить отель?' : 'Включить отель?'}
        text={
          hotel?.status === 'active'
            ? 'Все будущие брони будут отменены автоматически. Это действие нельзя откатить для уже отменённых броней.'
            : 'Отель снова появится в каталоге.'
        }
        confirmText={hotel?.status === 'active' ? 'Отключить' : 'Включить'}
        danger={hotel?.status === 'active'}
        loading={statusState.isLoading}
        onConfirm={toggleStatus}
        onClose={() => setConfirmStatus(false)}
      >
        {statusState.error && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {parseError(statusState.error)?.message}
          </Alert>
        )}
      </ConfirmDialog>
      <Snackbar open={Boolean(toast)} autoHideDuration={5000} onClose={() => setToast('')} message={toast} />
    </>
  );
}

function ManagersCard({
  hotelId,
  managerIds,
  onToast,
}: {
  hotelId: string;
  managerIds: string[];
  onToast: (s: string) => void;
}) {
  const { data: managers } = useUsersQuery({ role: 'hotel_manager', page: 1 });
  const [assign, assignState] = useAssignManagerMutation();
  const [remove] = useRemoveManagerMutation();
  const [pick, setPick] = useState<User | null>(null);
  const byId = new Map((managers?.items ?? []).map((u) => [u.id, u]));
  const options = (managers?.items ?? []).filter((u) => !managerIds.includes(u.id));
  const err = parseError(assignState.error);

  return (
    <Card>
      <CardContent sx={{ p: 3 }}>
        <Typography variant="h4" sx={{ mb: 2 }}>
          Менеджеры отеля
        </Typography>
        <Stack spacing={1} sx={{ mb: 2 }}>
          {managerIds.length === 0 && <Typography color="text.secondary">Менеджер не назначен</Typography>}
          {managerIds.map((mid) => (
            <Stack key={mid} direction="row" alignItems="center" justifyContent="space-between">
              <Box>
                <Typography>{byId.get(mid)?.full_name ?? 'Пользователь'}</Typography>
                <Typography variant="caption" color="text.secondary">
                  {byId.get(mid)?.email ?? mid}
                </Typography>
              </Box>
              <Tooltip title="Снять с отеля">
                <IconButton
                  onClick={() =>
                    remove({ hotelId, userId: mid })
                      .unwrap()
                      .then(() => onToast('Менеджер снят'))
                  }
                >
                  <PersonRemoveOutlinedIcon />
                </IconButton>
              </Tooltip>
            </Stack>
          ))}
        </Stack>
        <Stack direction="row" spacing={1}>
          <Autocomplete
            sx={{ flexGrow: 1 }}
            size="small"
            options={options}
            value={pick}
            onChange={(_, v) => setPick(v)}
            getOptionLabel={(u) => `${u.full_name} (${u.email})`}
            renderInput={(p) => <TextField {...p} label="Назначить менеджера" />}
            noOptionsText="Нет свободных менеджеров — выдайте роль в «Пользователях»"
          />
          <Button
            variant="contained"
            disabled={!pick || assignState.isLoading}
            onClick={() =>
              pick &&
              assign({ hotelId, userId: pick.id })
                .unwrap()
                .then(() => {
                  setPick(null);
                  onToast('Менеджер назначен');
                })
                .catch(() => undefined)
            }
          >
            Добавить
          </Button>
        </Stack>
        {err && (
          <Alert severity={err.code === 'identity_unavailable' ? 'warning' : 'error'} sx={{ mt: 2 }}>
            {err.message}
          </Alert>
        )}
      </CardContent>
    </Card>
  );
}

function RoomTypesCard({
  hotelId,
  rooms,
  onToast,
}: {
  hotelId: string;
  rooms: RoomType[];
  onToast: (s: string) => void;
}) {
  const [setStatus] = useSetRoomStatusMutation();
  const toggle = (r: RoomType) =>
    setStatus({
      id: r.id,
      hotelId,
      status: r.status === 'active' ? 'archived' : 'active',
      version: r.version,
    })
      .unwrap()
      .then(() =>
        onToast(
          r.status === 'active' ? 'Категория в архиве — новые брони невозможны' : 'Категория снова продаётся',
        ),
      )
      .catch((e) => onToast(parseError(e)?.message ?? 'Ошибка'));

  return (
    <Card>
      <CardContent sx={{ p: 3 }}>
        <Stack direction="row" justifyContent="space-between" alignItems="center" sx={{ mb: 2 }}>
          <Typography variant="h4">Категории номеров</Typography>
          <Button variant="contained" component={RouterLink} to={`/manage/hotels/${hotelId}/room-types/new`}>
            Добавить категорию
          </Button>
        </Stack>
        {rooms.length === 0 ? (
          <Typography color="text.secondary">
            Пока нет ни одной категории — без них отель нельзя забронировать.
          </Typography>
        ) : (
          <Box sx={{ overflowX: 'auto' }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Название</TableCell>
                  <TableCell align="right">Гостей</TableCell>
                  <TableCell align="right">Номеров</TableCell>
                  <TableCell align="right">Цена / ночь</TableCell>
                  <TableCell>Статус</TableCell>
                  <TableCell />
                </TableRow>
              </TableHead>
              <TableBody>
                {rooms.map((r) => (
                  <TableRow key={r.id} hover>
                    <TableCell>{r.name}</TableCell>
                    <TableCell align="right">{r.capacity}</TableCell>
                    <TableCell align="right">{r.total_rooms}</TableCell>
                    <TableCell align="right">{formatMoney(r.price)}</TableCell>
                    <TableCell>
                      <Chip
                        size="small"
                        label={r.status === 'active' ? 'В продаже' : 'Архив'}
                        color={r.status === 'active' ? 'success' : 'default'}
                      />
                    </TableCell>
                    <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                      <Tooltip title="Редактировать">
                        <span>
                          <IconButton
                            component={RouterLink}
                            to={`/manage/room-types/${r.id}`}
                            disabled={r.status !== 'active'}
                          >
                            <EditOutlinedIcon />
                          </IconButton>
                        </span>
                      </Tooltip>
                      <Tooltip title={r.status === 'active' ? 'В архив' : 'Вернуть в продажу'}>
                        <IconButton onClick={() => toggle(r)}>
                          {r.status === 'active' ? <Inventory2OutlinedIcon /> : <UnarchiveOutlinedIcon />}
                        </IconButton>
                      </Tooltip>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Box>
        )}
      </CardContent>
    </Card>
  );
}
