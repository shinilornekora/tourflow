// Сессия хранится только в памяти (Redux). Access-токен не кладётся в
// localStorage: XSS не сможет его украсть «на потом». После перезагрузки
// страницы сессия восстанавливается через POST /auth/refresh — refresh-токен
// лежит в HttpOnly-cookie, недоступной JavaScript.
import { createSlice, type PayloadAction } from '@reduxjs/toolkit';
import type { Session, User } from '@/shared/api/types';

export type AuthStatus = 'unknown' | 'authenticated' | 'anonymous';

interface AuthState {
  status: AuthStatus;
  token: string | null;
  user: User | null;
}

// Флаг «в этом браузере была сессия». Сам по себе ничего не даёт (токен —
// в HttpOnly-cookie), но избавляет гостя от заведомо лишнего POST /refresh
// с ответом 401 на каждой загрузке страницы.
export const SESSION_HINT = 'tf_had_session';

export function setSessionHint(on: boolean) {
  try {
    if (on) localStorage.setItem(SESSION_HINT, '1');
    else localStorage.removeItem(SESSION_HINT);
  } catch {
    /* приватный режим: просто будем всегда пробовать refresh */
  }
}

export function hasSessionHint(): boolean {
  try {
    return localStorage.getItem(SESSION_HINT) !== null;
  } catch {
    return true;
  }
}

const initialState: AuthState = { status: 'unknown', token: null, user: null };

const authSlice = createSlice({
  name: 'auth',
  initialState,
  reducers: {
    sessionReceived(state, action: PayloadAction<Session>) {
      state.status = 'authenticated';
      state.token = action.payload.access_token;
      state.user = action.payload.user;
    },
    userUpdated(state, action: PayloadAction<User>) {
      state.user = action.payload;
    },
    loggedOut(state) {
      state.status = 'anonymous';
      state.token = null;
      state.user = null;
    },
  },
});

export const { sessionReceived, userUpdated, loggedOut } = authSlice.actions;
export default authSlice.reducer;
