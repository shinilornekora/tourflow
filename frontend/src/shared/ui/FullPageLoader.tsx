import { Box, CircularProgress } from '@mui/material';

export function FullPageLoader() {
  return (
    <Box
      sx={{ minHeight: '50vh', display: 'grid', placeItems: 'center' }}
      role="status"
      aria-label="Загрузка"
    >
      <CircularProgress />
    </Box>
  );
}
