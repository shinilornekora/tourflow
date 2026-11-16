// Базовый RTK Query API. Все эндпоинты внедряются в него (injectEndpoints),
// поэтому у приложения один кэш и одна система тегов инвалидации.
//
// Обновление токена: access-токен живёт 15 минут. Если сервер ответил 401,
// мы один раз вызываем /auth/refresh и повторяем исходный запрос. Несколько
// параллельных запросов, получивших 401, ждут ОДНОГО refresh (общий промис),
// иначе вторая попытка предъявила бы уже отозванный refresh-токен и сервер
// принял бы это за кражу и отозвал всю сессию.
import {
  createApi,
  fetchBaseQuery,
  type BaseQueryFn,
  type FetchArgs,
  type FetchBaseQueryError,
} from '@reduxjs/toolkit/query/react';
import type { Session } from './types';
import { loggedOut, sessionReceived } from '@/features/auth/authSlice';

interface StateWithAuth {
  auth: { token: string | null };
}

const rawBaseQuery = fetchBaseQuery({
  baseUrl: '/api',
  credentials: 'same-origin',
  prepareHeaders: (headers, { getState }) => {
    const token = (getState() as StateWithAuth).auth.token;
    if (token) headers.set('Authorization', `Bearer ${token}`);
    return headers;
  },
});

let refreshing: Promise<Session | null> | null = null;

async function refreshSession(): Promise<Session | null> {
  const res = await fetch('/api/auth/refresh', { method: 'POST', credentials: 'same-origin' });
  if (!res.ok) return null;
  return (await res.json()) as Session;
}

/** Один refresh на всё приложение в каждый момент времени. */
export function refreshOnce(): Promise<Session | null> {
  if (!refreshing) {
    refreshing = refreshSession().finally(() => {
      refreshing = null;
    });
  }
  return refreshing;
}

const baseQueryWithReauth: BaseQueryFn<string | FetchArgs, unknown, FetchBaseQueryError> = async (
  args,
  api,
  extra,
) => {
  let result = await rawBaseQuery(args, api, extra);
  const url = typeof args === 'string' ? args : args.url;
  const hadToken = Boolean((api.getState() as StateWithAuth).auth.token);
  if (result.error?.status === 401 && hadToken && !url.startsWith('/auth/')) {
    const session = await refreshOnce();
    if (session) {
      api.dispatch(sessionReceived(session));
      result = await rawBaseQuery(args, api, extra);
    } else {
      api.dispatch(loggedOut());
    }
  }
  return result;
};

export const baseApi = createApi({
  reducerPath: 'api',
  baseQuery: baseQueryWithReauth,
  tagTypes: [
    'Me',
    'Users',
    'Hotel',
    'HotelList',
    'RoomType',
    'Availability',
    'Reservation',
    'Notification',
    'Audit',
  ],
  endpoints: () => ({}),
});
