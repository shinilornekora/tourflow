import { useState, type ReactNode } from 'react';
import { Link as RouterLink, NavLink, Outlet, useNavigate } from 'react-router-dom';
import {
  AppBar,
  Avatar,
  Badge,
  Box,
  Button,
  Container,
  Divider,
  Drawer,
  IconButton,
  List,
  ListItemButton,
  ListItemText,
  Menu,
  MenuItem,
  Stack,
  Toolbar,
  Tooltip,
  Typography,
  useMediaQuery,
  useTheme,
} from '@mui/material';
import MenuIcon from '@mui/icons-material/Menu';
import NotificationsOutlinedIcon from '@mui/icons-material/NotificationsOutlined';
import { Suspense } from 'react';
import { useAppDispatch, useAppSelector } from '@/app/store';
import { loggedOut } from '@/features/auth/authSlice';
import { api, useLogoutMutation, useUnreadCountQuery } from '@/shared/api/endpoints';
import type { Role } from '@/shared/api/types';
import { roleLabel } from '@/shared/lib/format';
import { FullPageLoader } from '@/shared/ui/FullPageLoader';

interface NavItem {
  to: string;
  label: string;
  roles?: Role[]; // undefined — всем, включая анонимов
}

// Меню зависит от роли: у администратора появляется управление
// пользователями, у менеджера — его отели и брони, у клиента — «корзина».
const nav: NavItem[] = [
  { to: '/', label: 'Отели' },
  { to: '/my/bookings', label: 'Мои брони', roles: ['customer'] },
  { to: '/manage/hotels', label: 'Мои отели', roles: ['hotel_manager'] },
  { to: '/manage/hotels', label: 'Управление отелями', roles: ['admin'] },
  { to: '/manage/bookings', label: 'Брони отелей', roles: ['hotel_manager', 'admin'] },
  { to: '/admin/users', label: 'Пользователи', roles: ['admin'] },
];

function Logo() {
  return (
    <Stack
      component={RouterLink}
      to="/"
      direction="row"
      alignItems="center"
      spacing={1}
      sx={{ color: 'inherit', textDecoration: 'none' }}
    >
      <Box component="svg" viewBox="0 0 32 32" sx={{ width: 30, height: 30 }} aria-hidden>
        <rect width="32" height="32" rx="8" fill="#f4c95d" />
        <path d="M8 22V12l8-5 8 5v10h-5v-6h-6v6z" fill="#0f5c63" />
      </Box>
      <Typography
        sx={{ fontFamily: 'Georgia, serif', fontWeight: 700, fontSize: '1.3rem', letterSpacing: '-0.01em' }}
      >
        TourFlow
      </Typography>
    </Stack>
  );
}

function NotificationsBell() {
  // Опрос раз в 30 секунд — проще WebSocket/SSE и достаточно для уведомлений
  // о бронях (см. TRADEOFFS: «polling вместо push»).
  const { data } = useUnreadCountQuery(undefined, { pollingInterval: 30_000, skipPollingIfUnfocused: true });
  const navigate = useNavigate();
  return (
    <Tooltip title="Уведомления">
      <IconButton color="inherit" onClick={() => navigate('/notifications')} aria-label="Уведомления">
        <Badge badgeContent={data?.unread ?? 0} color="secondary">
          <NotificationsOutlinedIcon />
        </Badge>
      </IconButton>
    </Tooltip>
  );
}

function UserMenu() {
  const user = useAppSelector((s) => s.auth.user)!;
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [logout] = useLogoutMutation();
  const dispatch = useAppDispatch();
  const navigate = useNavigate();
  const onLogout = async () => {
    setAnchor(null);
    await logout()
      .unwrap()
      .catch(() => undefined);
    dispatch(loggedOut());
    dispatch(api.util.resetApiState()); // чужие данные не должны остаться в кэше
    navigate('/');
  };
  return (
    <>
      <IconButton onClick={(e) => setAnchor(e.currentTarget)} aria-label="Меню пользователя">
        <Avatar
          sx={{
            width: 34,
            height: 34,
            bgcolor: 'secondary.main',
            color: 'secondary.contrastText',
            fontWeight: 700,
          }}
        >
          {user.full_name.slice(0, 1).toUpperCase()}
        </Avatar>
      </IconButton>
      <Menu anchorEl={anchor} open={Boolean(anchor)} onClose={() => setAnchor(null)}>
        <Box sx={{ px: 2, py: 1 }}>
          <Typography fontWeight={700}>{user.full_name}</Typography>
          <Typography variant="body2" color="text.secondary">
            {roleLabel[user.role]}
          </Typography>
        </Box>
        <Divider />
        <MenuItem component={RouterLink} to="/profile" onClick={() => setAnchor(null)}>
          Личный кабинет
        </MenuItem>
        <MenuItem onClick={onLogout}>Выйти</MenuItem>
      </Menu>
    </>
  );
}

function NavButton({ to, children }: { to: string; children: ReactNode }) {
  return (
    <Button
      component={NavLink}
      to={to}
      end={to === '/'}
      color="inherit"
      sx={{
        opacity: 0.85,
        '&.active': { opacity: 1, boxShadow: 'inset 0 -2px 0 #f4c95d', borderRadius: 0 },
      }}
    >
      {children}
    </Button>
  );
}

export function Layout() {
  const { status, user } = useAppSelector((s) => s.auth);
  const theme = useTheme();
  const mobile = useMediaQuery(theme.breakpoints.down('md'));
  const [drawer, setDrawer] = useState(false);
  const items = nav.filter((n) => !n.roles || (user && n.roles.includes(user.role)));

  return (
    <Box sx={{ minHeight: '100vh', display: 'flex', flexDirection: 'column' }}>
      <AppBar position="sticky" color="primary" elevation={0}>
        <Container maxWidth="lg">
          <Toolbar disableGutters sx={{ gap: 2 }}>
            {mobile && (
              <IconButton color="inherit" onClick={() => setDrawer(true)} aria-label="Открыть меню">
                <MenuIcon />
              </IconButton>
            )}
            <Logo />
            {!mobile && (
              <Stack direction="row" spacing={0.5} sx={{ ml: 3 }} component="nav">
                {items.map((n) => (
                  <NavButton key={n.to + n.label} to={n.to}>
                    {n.label}
                  </NavButton>
                ))}
              </Stack>
            )}
            <Box sx={{ flexGrow: 1 }} />
            {status === 'authenticated' && (
              <>
                <NotificationsBell />
                <UserMenu />
              </>
            )}
            {status === 'anonymous' && (
              <Stack direction="row" spacing={1}>
                <Button color="inherit" component={RouterLink} to="/login">
                  Войти
                </Button>
                <Button
                  variant="contained"
                  color="secondary"
                  component={RouterLink}
                  to="/register"
                  sx={{ display: { xs: 'none', sm: 'inline-flex' } }}
                >
                  Регистрация
                </Button>
              </Stack>
            )}
          </Toolbar>
        </Container>
      </AppBar>

      <Drawer open={drawer} onClose={() => setDrawer(false)}>
        <List sx={{ width: 260 }} onClick={() => setDrawer(false)}>
          {items.map((n) => (
            <ListItemButton key={n.to + n.label} component={NavLink} to={n.to} end={n.to === '/'}>
              <ListItemText primary={n.label} />
            </ListItemButton>
          ))}
        </List>
      </Drawer>

      <Container component="main" maxWidth="lg" sx={{ py: { xs: 3, md: 5 }, flexGrow: 1 }}>
        <Suspense fallback={<FullPageLoader />}>
          <Outlet />
        </Suspense>
      </Container>

      <Box
        component="footer"
        sx={{ py: 3, borderTop: 1, borderColor: 'divider', bgcolor: 'background.paper' }}
      >
        <Container maxWidth="lg">
          <Typography variant="body2" color="text.secondary">
            TourFlow — учебный эталон-сервис бронирования отелей · РУТ (МИИТ), 2026
          </Typography>
        </Container>
      </Box>
    </Box>
  );
}
