import { useEffect } from 'react';
import { useAppDispatch } from '@/app/store';
import { refreshOnce } from '@/shared/api/baseApi';
import { hasSessionHint, loggedOut, sessionReceived } from './authSlice';

/**
 * При загрузке SPA пробуем восстановить сессию по HttpOnly-cookie.
 * Пока ответа нет, статус 'unknown' — защищённые маршруты показывают
 * загрузку, а не мигают редиректом на /login.
 */
export function useSessionBootstrap() {
  const dispatch = useAppDispatch();
  useEffect(() => {
    let cancelled = false;
    if (!hasSessionHint()) {
      dispatch(loggedOut());
      return;
    }
    refreshOnce()
      .then((s) => {
        if (cancelled) return;
        dispatch(s ? sessionReceived(s) : loggedOut());
      })
      .catch(() => !cancelled && dispatch(loggedOut()));
    return () => {
      cancelled = true;
    };
  }, [dispatch]);
}
