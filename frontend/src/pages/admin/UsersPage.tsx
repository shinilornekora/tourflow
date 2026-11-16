import { useState } from 'react';
import {
  Alert,
  Box,
  Card,
  MenuItem,
  Skeleton,
  Snackbar,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  TextField,
} from '@mui/material';
import { useAppSelector } from '@/app/store';
import { useChangeRoleMutation, useUsersQuery } from '@/shared/api/endpoints';
import type { Role } from '@/shared/api/types';
import { formatDateTime, parseError, roleLabel } from '@/shared/lib/format';
import { ErrorAlert, PageHeader, Pager } from '@/shared/ui/common';
import { useDebounced } from '@/shared/lib/useDebounced';

const roles: Role[] = ['customer', 'hotel_manager', 'admin'];

export default function UsersPage() {
  const me = useAppSelector((s) => s.auth.user);
  const [q, setQ] = useState('');
  const [role, setRole] = useState<Role | ''>('');
  const [page, setPage] = useState(1);
  const dq = useDebounced(q);
  const { data, isLoading, error, refetch } = useUsersQuery({ q: dq, role, page });
  const [changeRole, cr] = useChangeRoleMutation();
  const [toast, setToast] = useState('');

  const onChange = async (id: string, r: Role) => {
    try {
      await changeRole({ id, role: r }).unwrap();
      setToast(`Роль изменена на «${roleLabel[r]}». Пользователю нужно войти заново.`);
    } catch {
      /* ниже */
    }
  };

  return (
    <>
      <PageHeader title="Пользователи" subtitle="Выдача ролей: клиент, менеджер отеля, администратор" />
      <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} sx={{ mb: 3 }}>
        <TextField
          label="Поиск по имени или email"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setPage(1);
          }}
        />
        <TextField
          select
          label="Роль"
          value={role}
          onChange={(e) => {
            setRole(e.target.value as Role | '');
            setPage(1);
          }}
          sx={{ minWidth: 220 }}
        >
          <MenuItem value="">Все роли</MenuItem>
          {roles.map((r) => (
            <MenuItem key={r} value={r}>
              {roleLabel[r]}
            </MenuItem>
          ))}
        </TextField>
      </Stack>
      {cr.error && (
        <Alert severity="error" sx={{ mb: 2 }}>
          {parseError(cr.error)?.message}
        </Alert>
      )}
      <ErrorAlert error={error} onRetry={refetch} />
      {isLoading && <Skeleton variant="rounded" height={300} />}
      {data && (
        <Card>
          <Box sx={{ overflowX: 'auto' }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Имя</TableCell>
                  <TableCell>Email</TableCell>
                  <TableCell>Телефон</TableCell>
                  <TableCell>Зарегистрирован</TableCell>
                  <TableCell sx={{ minWidth: 200 }}>Роль</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {data.items.map((u) => (
                  <TableRow key={u.id} hover>
                    <TableCell>{u.full_name}</TableCell>
                    <TableCell>{u.email}</TableCell>
                    <TableCell>{u.phone || '—'}</TableCell>
                    <TableCell sx={{ whiteSpace: 'nowrap' }}>{formatDateTime(u.created_at)}</TableCell>
                    <TableCell>
                      <TextField
                        select
                        value={u.role}
                        disabled={u.id === me?.id || cr.isLoading}
                        helperText={u.id === me?.id ? 'Это вы' : undefined}
                        onChange={(e) => onChange(u.id, e.target.value as Role)}
                      >
                        {roles.map((r) => (
                          <MenuItem key={r} value={r}>
                            {roleLabel[r]}
                          </MenuItem>
                        ))}
                      </TextField>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Box>
        </Card>
      )}
      {data && <Pager page={page} total={data.total} pageSize={data.page_size} onChange={setPage} />}
      <Snackbar open={Boolean(toast)} autoHideDuration={5000} onClose={() => setToast('')} message={toast} />
    </>
  );
}
