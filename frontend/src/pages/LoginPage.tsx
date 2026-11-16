import { useState, type FormEvent } from 'react';
import { Link as RouterLink, useLocation, useNavigate } from 'react-router-dom';
import { Alert, Box, Button, Card, CardContent, Link, Stack, TextField, Typography } from '@mui/material';
import { useAppDispatch } from '@/app/store';
import { sessionReceived } from '@/features/auth/authSlice';
import { useLoginMutation } from '@/shared/api/endpoints';
import { parseError } from '@/shared/lib/format';

export function AuthCard({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle: string;
  children: React.ReactNode;
}) {
  return (
    <Box sx={{ display: 'grid', placeItems: 'center', py: { xs: 2, md: 6 } }}>
      <Card sx={{ width: '100%', maxWidth: 440 }}>
        <CardContent sx={{ p: { xs: 3, sm: 4 } }}>
          <Typography variant="h3" component="h1">
            {title}
          </Typography>
          <Typography color="text.secondary" sx={{ mt: 0.5, mb: 3 }}>
            {subtitle}
          </Typography>
          {children}
        </CardContent>
      </Card>
    </Box>
  );
}

export default function LoginPage() {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [login, { isLoading, error }] = useLoginMutation();
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const location = useLocation();
  const from = (location.state as { from?: string } | null)?.from ?? '/';
  const e = parseError(error);

  const onSubmit = async (ev: FormEvent) => {
    ev.preventDefault();
    try {
      const session = await login({ email, password }).unwrap();
      dispatch(sessionReceived(session));
      navigate(from, { replace: true });
    } catch {
      /* ошибка показана из состояния мутации */
    }
  };

  return (
    <AuthCard title="Вход" subtitle="Чтобы бронировать номера и видеть свои поездки">
      <Stack component="form" spacing={2} onSubmit={onSubmit} noValidate>
        {e && (
          <Alert severity="error">
            {e.status === 429 ? 'Слишком много попыток, подождите минуту' : e.message}
          </Alert>
        )}
        <TextField
          label="Email"
          type="email"
          autoComplete="email"
          value={email}
          onChange={(x) => setEmail(x.target.value)}
          required
          autoFocus
        />
        <TextField
          label="Пароль"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(x) => setPassword(x.target.value)}
          required
        />
        <Button type="submit" variant="contained" size="large" disabled={isLoading || !email || !password}>
          {isLoading ? 'Входим…' : 'Войти'}
        </Button>
        <Typography variant="body2" color="text.secondary" textAlign="center">
          Нет аккаунта?{' '}
          <Link component={RouterLink} to="/register" state={location.state}>
            Зарегистрироваться
          </Link>
        </Typography>
      </Stack>
    </AuthCard>
  );
}
