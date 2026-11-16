import { configureStore } from '@reduxjs/toolkit';
import { useDispatch, useSelector } from 'react-redux';
import { baseApi } from '@/shared/api/baseApi';
import authReducer, { setSessionHint } from '@/features/auth/authSlice';

export const store = configureStore({
  reducer: {
    auth: authReducer,
    [baseApi.reducerPath]: baseApi.reducer,
  },
  middleware: (getDefault) => getDefault().concat(baseApi.middleware),
});

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;

export const useAppDispatch = useDispatch.withTypes<AppDispatch>();
export const useAppSelector = useSelector.withTypes<RootState>();

// Reducer'ы чистые, поэтому запись подсказки о сессии в localStorage —
// здесь, в подписке на изменение статуса.
let lastStatus = store.getState().auth.status;
store.subscribe(() => {
  const status = store.getState().auth.status;
  if (status === lastStatus) return;
  lastStatus = status;
  if (status !== 'unknown') setSessionHint(status === 'authenticated');
});
