import { useState, type FormEvent } from 'react';
import { Link as RouterLink, useLocation, useNavigate } from 'react-router-dom';
import { Alert, Button, Link, Stack, TextField, Typography } from '@mui/material';
import { useAppDispatch } from '@/app/store';
import { sessionReceived } from '@/features/auth/authSlice';
import { useRegisterMutation } from '@/shared/api/endpoints';
import { parseError } from '@/shared/lib/format';
import { AuthCard } from './LoginPage';

// Клиентская валидация дублирует серверную только для мгновенной подсказки.
// Источник истины — сервер: его ошибки по полям показываются под полями.
function validate(v: { email: string; password: string; full_name: string }) {
  const f: Record<string, string> = {};
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v.email.trim())) f.email = 'Некорректный email';
  if (v.password.length < 8) f.password = 'Минимум 8 символов';
  else if (!/\d/.test(v.password) || !/\p{L}/u.test(v.password)) f.password = 'Нужны буква и цифра';
  if (v.full_name.trim().length < 2) f.full_name = 'Укажите имя';
  return f;
}

export default function RegisterPage() {
  const [form, setForm] = useState({ email: '', password: '', full_name: '', phone: '' });
  const [touched, setTouched] = useState(false);
  const [register, { isLoading, error }] = useRegisterMutation();
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const location = useLocation();
  const from = (location.state as { from?: string } | null)?.from ?? '/';
  const server = parseError(error);
  const local = touched ? validate(form) : {};
  const fieldError = (k: string) => local[k] ?? server?.fields[k];
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [k]: e.target.value });

  const onSubmit = async (ev: FormEvent) => {
    ev.preventDefault();
    setTouched(true);
    if (Object.keys(validate(form)).length) return;
    try {
      const s = await register(form).unwrap();
      dispatch(sessionReceived(s));
      navigate(from, { replace: true });
    } catch {
      /* ошибки под полями */
    }
  };

  return (
    <AuthCard title="Регистрация" subtitle="Аккаунт клиента — бронируйте отели в пару кликов">
      <Stack component="form" spacing={2} onSubmit={onSubmit} noValidate>
        {server && !Object.keys(server.fields).length && <Alert severity="error">{server.message}</Alert>}
        <TextField
          label="Имя и фамилия"
          autoComplete="name"
          value={form.full_name}
          onChange={set('full_name')}
          error={!!fieldError('full_name')}
          helperText={fieldError('full_name')}
          required
        />
        <TextField
          label="Email"
          type="email"
          autoComplete="email"
          value={form.email}
          onChange={set('email')}
          error={!!fieldError('email')}
          helperText={fieldError('email')}
          required
        />
        <TextField
          label="Телефон (необязательно)"
          type="tel"
          autoComplete="tel"
          value={form.phone}
          onChange={set('phone')}
          error={!!fieldError('phone')}
          helperText={fieldError('phone') ?? 'Например, +7 900 123-45-67'}
        />
        <TextField
          label="Пароль"
          type="password"
          autoComplete="new-password"
          value={form.password}
          onChange={set('password')}
          error={!!fieldError('password')}
          helperText={fieldError('password') ?? 'Не меньше 8 символов, буквы и цифры'}
          required
        />
        <Button type="submit" variant="contained" size="large" disabled={isLoading}>
          {isLoading ? 'Создаём аккаунт…' : 'Зарегистрироваться'}
        </Button>
        <Typography variant="body2" color="text.secondary" textAlign="center">
          Уже есть аккаунт?{' '}
          <Link component={RouterLink} to="/login" state={location.state}>
            Войти
          </Link>
        </Typography>
      </Stack>
    </AuthCard>
  );
}
