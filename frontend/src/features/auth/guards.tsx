// Защита маршрутов (обязательное требование курсовой):
//  - RequireAuth: неавторизованного отправляем на /login и запоминаем,
//    куда он шёл, чтобы вернуть после входа;
//  - RequireRole: авторизованный, но без нужной роли получает 403-экран
//    (а не редирект на вход — он уже вошёл, проблема в правах).
// Это UX-слой. Настоящую проверку прав делает бэкенд на каждом запросе.
import type { ReactNode } from 'react';
import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { useAppSelector } from '@/app/store';
import type { Role } from '@/shared/api/types';
import { FullPageLoader } from '@/shared/ui/FullPageLoader';
import { ForbiddenPage } from '@/pages/ForbiddenPage';

export function RequireAuth({ children }: { children?: ReactNode }) {
  const status = useAppSelector((s) => s.auth.status);
  const location = useLocation();
  if (status === 'unknown') return <FullPageLoader />;
  if (status === 'anonymous') {
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />;
  }
  return children ? <>{children}</> : <Outlet />;
}

export function RequireRole({ roles, children }: { roles: Role[]; children?: ReactNode }) {
  const user = useAppSelector((s) => s.auth.user);
  if (!user || !roles.includes(user.role)) return <ForbiddenPage />;
  return children ? <>{children}</> : <Outlet />;
}

/** Страницы входа/регистрации: авторизованного уводим на главную. */
export function GuestOnly({ children }: { children: ReactNode }) {
  const status = useAppSelector((s) => s.auth.status);
  if (status === 'unknown') return <FullPageLoader />;
  if (status === 'authenticated') return <Navigate to="/" replace />;
  return <>{children}</>;
}

export function useRole(): Role | null {
  return useAppSelector((s) => s.auth.user?.role ?? null);
}
