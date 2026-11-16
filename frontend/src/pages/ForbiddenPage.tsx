import { Link as RouterLink } from 'react-router-dom';
import { Box, Button, Typography } from '@mui/material';

export function ForbiddenPage() {
  return (
    <Box sx={{ textAlign: 'center', py: 10 }}>
      <Typography
        sx={{
          fontFamily: 'Georgia, serif',
          fontSize: '5rem',
          fontWeight: 700,
          color: 'primary.main',
          lineHeight: 1,
        }}
      >
        403
      </Typography>
      <Typography variant="h3" component="h1" sx={{ mt: 2 }}>
        Недостаточно прав
      </Typography>
      <Typography color="text.secondary" sx={{ mt: 1 }}>
        Этот раздел доступен другой роли. Если вам нужен доступ — обратитесь к администратору.
      </Typography>
      <Button variant="contained" component={RouterLink} to="/" sx={{ mt: 4 }}>
        На главную
      </Button>
    </Box>
  );
}
