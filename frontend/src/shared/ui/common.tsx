import type { ReactNode } from 'react';
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Pagination,
  Stack,
  Typography,
} from '@mui/material';
import type { FetchBaseQueryError } from '@reduxjs/toolkit/query';
import type { SerializedError } from '@reduxjs/toolkit';
import { parseError } from '@/shared/lib/format';

export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: string;
  subtitle?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <Stack
      direction={{ xs: 'column', sm: 'row' }}
      justifyContent="space-between"
      alignItems={{ xs: 'flex-start', sm: 'flex-end' }}
      spacing={2}
      sx={{ mb: 3 }}
    >
      <Box>
        <Typography variant="h2" component="h1">
          {title}
        </Typography>
        {subtitle && (
          <Typography color="text.secondary" component="div" sx={{ mt: 0.5 }}>
            {subtitle}
          </Typography>
        )}
      </Box>
      {actions && (
        <Stack direction="row" spacing={1}>
          {actions}
        </Stack>
      )}
    </Stack>
  );
}

export function ErrorAlert({
  error,
  title,
  onRetry,
}: {
  error: FetchBaseQueryError | SerializedError | undefined;
  title?: string;
  onRetry?: () => void;
}) {
  const e = parseError(error);
  if (!e) return null;
  return (
    <Alert
      severity="error"
      sx={{ my: 2 }}
      action={
        onRetry && (
          <Button color="inherit" size="small" onClick={onRetry}>
            Повторить
          </Button>
        )
      }
    >
      {title && <AlertTitle>{title}</AlertTitle>}
      {e.message}
      {e.requestId && (
        <Typography variant="caption" component="div" sx={{ opacity: 0.7, mt: 0.5 }}>
          Код запроса для поддержки: {e.requestId}
        </Typography>
      )}
    </Alert>
  );
}

export function EmptyState({ title, text, action }: { title: string; text?: string; action?: ReactNode }) {
  return (
    <Box
      sx={{
        textAlign: 'center',
        py: 8,
        px: 2,
        border: '1px dashed',
        borderColor: 'divider',
        borderRadius: 3,
        bgcolor: 'background.paper',
      }}
    >
      <Typography variant="h4" component="p">
        {title}
      </Typography>
      {text && (
        <Typography color="text.secondary" sx={{ mt: 1, maxWidth: 480, mx: 'auto' }}>
          {text}
        </Typography>
      )}
      {action && <Box sx={{ mt: 3 }}>{action}</Box>}
    </Box>
  );
}

export function Pager({
  page,
  total,
  pageSize,
  onChange,
}: {
  page: number;
  total: number;
  pageSize: number;
  onChange: (page: number) => void;
}) {
  const count = Math.ceil(total / pageSize);
  if (count <= 1) return null;
  return (
    <Stack alignItems="center" sx={{ mt: 4 }}>
      <Pagination
        count={count}
        page={page}
        onChange={(_, p) => onChange(p)}
        color="primary"
        shape="rounded"
      />
    </Stack>
  );
}

export function ConfirmDialog({
  open,
  title,
  text,
  confirmText = 'Подтвердить',
  danger,
  loading,
  onConfirm,
  onClose,
  children,
}: {
  open: boolean;
  title: string;
  text?: ReactNode;
  confirmText?: string;
  danger?: boolean;
  loading?: boolean;
  onConfirm: () => void;
  onClose: () => void;
  children?: ReactNode;
}) {
  return (
    <Dialog open={open} onClose={loading ? undefined : onClose} maxWidth="xs" fullWidth>
      <DialogTitle>{title}</DialogTitle>
      <DialogContent>
        {text && <Typography color="text.secondary">{text}</Typography>}
        {children}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button onClick={onClose} disabled={loading}>
          Отмена
        </Button>
        <Button
          variant="contained"
          color={danger ? 'error' : 'primary'}
          onClick={onConfirm}
          disabled={loading}
        >
          {confirmText}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
