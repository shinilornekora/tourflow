import { Link as RouterLink } from 'react-router-dom';
import { Box, Button, Typography } from '@mui/material';

export default function NotFoundPage() {
  return (
    <Box sx={{ textAlign: 'center', py: 10 }}>
      <Typography
        sx={{
          fontFamily: 'Georgia, serif',
          fontSize: '6rem',
          fontWeight: 700,
          color: 'primary.main',
          lineHeight: 1,
        }}
      >
        404
      </Typography>
      <Typography variant="h3" component="h1" sx={{ mt: 2 }}>
        Такой страницы нет
      </Typography>
      <Typography color="text.secondary" sx={{ mt: 1 }}>
        Возможно, ссылка устарела или отель больше не принимает гостей.
      </Typography>
      <Button variant="contained" component={RouterLink} to="/" sx={{ mt: 4 }}>
        К списку отелей
      </Button>
    </Box>
  );
}
