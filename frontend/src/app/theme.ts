import { createTheme } from '@mui/material/styles';

// Палитра: глубокий бирюзовый (вода, море) + шафрановый акцент (солнце),
// тёплый «бумажный» фон. Заголовки — серифом, как в путеводителе.
export const theme = createTheme({
  palette: {
    mode: 'light',
    primary: { main: '#0f5c63', dark: '#0a4348', light: '#3f8a90', contrastText: '#ffffff' },
    secondary: { main: '#f4c95d', contrastText: '#2b2111' },
    background: { default: '#f7f5f0', paper: '#ffffff' },
    text: { primary: '#1f2a2c', secondary: '#5b6a6c' },
    success: { main: '#2e7d4f' },
    error: { main: '#b3261e' },
    divider: 'rgba(15, 92, 99, 0.12)',
  },
  shape: { borderRadius: 12 },
  typography: {
    fontFamily: '"Segoe UI", system-ui, -apple-system, Roboto, "Helvetica Neue", Arial, sans-serif',
    h1: {
      fontFamily: 'Georgia, "Times New Roman", serif',
      fontWeight: 700,
      fontSize: '2.6rem',
      letterSpacing: '-0.01em',
    },
    h2: { fontFamily: 'Georgia, "Times New Roman", serif', fontWeight: 700, fontSize: '2rem' },
    h3: { fontFamily: 'Georgia, "Times New Roman", serif', fontWeight: 700, fontSize: '1.6rem' },
    h4: { fontFamily: 'Georgia, "Times New Roman", serif', fontWeight: 700, fontSize: '1.35rem' },
    h5: { fontWeight: 700, fontSize: '1.1rem' },
    h6: { fontWeight: 700, fontSize: '1rem' },
    button: { textTransform: 'none', fontWeight: 600 },
  },
  components: {
    MuiButton: { defaultProps: { disableElevation: true } },
    MuiCard: {
      defaultProps: { elevation: 0 },
      styleOverrides: { root: { border: '1px solid rgba(15, 92, 99, 0.12)' } },
    },
    MuiPaper: { styleOverrides: { outlined: { borderColor: 'rgba(15, 92, 99, 0.12)' } } },
    MuiTextField: { defaultProps: { size: 'small', fullWidth: true } },
    MuiSelect: { defaultProps: { size: 'small' } },
    MuiChip: { styleOverrides: { root: { fontWeight: 600 } } },
  },
});
