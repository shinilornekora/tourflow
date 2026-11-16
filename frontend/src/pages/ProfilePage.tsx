import { useEffect, useState, type FormEvent } from 'react';
import {
  Alert,
  Button,
  Card,
  CardContent,
  Chip,
  Grid,
  Skeleton,
  Snackbar,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import { useAppDispatch } from '@/app/store';
import { userUpdated } from '@/features/auth/authSlice';
import { useChangePasswordMutation, useMeQuery, useUpdateMeMutation } from '@/shared/api/endpoints';
import { formatDateTime, parseError, roleLabel } from '@/shared/lib/format';
import { ErrorAlert, PageHeader } from '@/shared/ui/common';

// Личный кабинет есть у всех ролей: профиль и смена пароля.
export default function ProfilePage() {
  const { data: me, isLoading, error } = useMeQuery();
  const [update, upd] = useUpdateMeMutation();
  const [changePassword, pwd] = useChangePasswordMutation();
  const dispatch = useAppDispatch();
  const [form, setForm] = useState({ full_name: '', phone: '' });
  const [pw, setPw] = useState({ current_password: '', new_password: '' });
  const [toast, setToast] = useState('');

  useEffect(() => {
    if (me) setForm({ full_name: me.full_name, phone: me.phone });
  }, [me]);

  if (isLoading) return <Skeleton variant="rounded" height={300} />;
  if (!me) return <ErrorAlert error={error} />;

  const updErr = parseError(upd.error);
  const pwdErr = parseError(pwd.error);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    try {
      // version — оптимистическая блокировка: если профиль изменили в
      // другой вкладке, сервер вернёт 409, а не перезапишет молча.
      const u = await update({ ...form, version: me.version }).unwrap();
      dispatch(userUpdated(u));
      setToast('Профиль сохранён');
    } catch {
      /* ниже */
    }
  };

  const savePw = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await changePassword(pw).unwrap();
      setPw({ current_password: '', new_password: '' });
      setToast('Пароль изменён. Остальные устройства разлогинены.');
    } catch {
      /* ниже */
    }
  };

  return (
    <>
      <PageHeader
        title="Личный кабинет"
        subtitle={
          <>
            {me.email} · <Chip size="small" label={roleLabel[me.role]} /> · с нами с{' '}
            {formatDateTime(me.created_at)}
          </>
        }
      />
      <Grid container spacing={3}>
        <Grid item xs={12} md={7}>
          <Card>
            <CardContent component="form" onSubmit={save} sx={{ p: 3 }}>
              <Typography variant="h4" sx={{ mb: 2 }}>
                Профиль
              </Typography>
              <Stack spacing={2}>
                {updErr && !Object.keys(updErr.fields).length && (
                  <Alert severity={updErr.code === 'version_mismatch' ? 'warning' : 'error'}>
                    {updErr.message}
                  </Alert>
                )}
                <TextField
                  label="Имя и фамилия"
                  value={form.full_name}
                  onChange={(e) => setForm({ ...form, full_name: e.target.value })}
                  error={!!updErr?.fields.full_name}
                  helperText={updErr?.fields.full_name}
                />
                <TextField
                  label="Телефон"
                  value={form.phone}
                  onChange={(e) => setForm({ ...form, phone: e.target.value })}
                  error={!!updErr?.fields.phone}
                  helperText={updErr?.fields.phone}
                />
                <TextField
                  label="Email"
                  value={me.email}
                  disabled
                  helperText="Email изменить нельзя — это логин"
                />
                <Button
                  type="submit"
                  variant="contained"
                  disabled={upd.isLoading}
                  sx={{ alignSelf: 'flex-start' }}
                >
                  Сохранить
                </Button>
              </Stack>
            </CardContent>
          </Card>
        </Grid>
        <Grid item xs={12} md={5}>
          <Card>
            <CardContent component="form" onSubmit={savePw} sx={{ p: 3 }}>
              <Typography variant="h4" sx={{ mb: 2 }}>
                Смена пароля
              </Typography>
              <Stack spacing={2}>
                {pwdErr && !Object.keys(pwdErr.fields).length && (
                  <Alert severity="error">{pwdErr.message}</Alert>
                )}
                <TextField
                  label="Текущий пароль"
                  type="password"
                  autoComplete="current-password"
                  value={pw.current_password}
                  onChange={(e) => setPw({ ...pw, current_password: e.target.value })}
                  error={!!pwdErr?.fields.current_password}
                  helperText={pwdErr?.fields.current_password}
                />
                <TextField
                  label="Новый пароль"
                  type="password"
                  autoComplete="new-password"
                  value={pw.new_password}
                  onChange={(e) => setPw({ ...pw, new_password: e.target.value })}
                  error={!!pwdErr?.fields.password}
                  helperText={pwdErr?.fields.password ?? 'Не меньше 8 символов, буквы и цифры'}
                />
                <Button
                  type="submit"
                  variant="outlined"
                  disabled={pwd.isLoading || !pw.current_password || !pw.new_password}
                  sx={{ alignSelf: 'flex-start' }}
                >
                  Изменить пароль
                </Button>
              </Stack>
            </CardContent>
          </Card>
        </Grid>
      </Grid>
      <Snackbar open={Boolean(toast)} autoHideDuration={4000} onClose={() => setToast('')} message={toast} />
    </>
  );
}
