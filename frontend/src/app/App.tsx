import { lazy } from 'react';
import { createBrowserRouter, RouterProvider } from 'react-router-dom';
import { Layout } from './Layout';
import { GuestOnly, RequireAuth, RequireRole } from '@/features/auth/guards';
import { useSessionBootstrap } from '@/features/auth/useSessionBootstrap';

// Каждая страница — отдельный чанк: на первом экране грузится только то,
// что нужно для каталога. Админские страницы клиент не скачивает вообще.
const HotelsPage = lazy(() => import('@/pages/HotelsPage'));
const HotelPage = lazy(() => import('@/pages/HotelPage'));
const LoginPage = lazy(() => import('@/pages/LoginPage'));
const RegisterPage = lazy(() => import('@/pages/RegisterPage'));
const ProfilePage = lazy(() => import('@/pages/ProfilePage'));
const MyBookingsPage = lazy(() => import('@/pages/MyBookingsPage'));
const NotificationsPage = lazy(() => import('@/pages/NotificationsPage'));
const ManageHotelsPage = lazy(() => import('@/pages/manage/ManageHotelsPage'));
const HotelEditPage = lazy(() => import('@/pages/manage/HotelEditPage'));
const RoomTypeEditPage = lazy(() => import('@/pages/manage/RoomTypeEditPage'));
const ManageBookingsPage = lazy(() => import('@/pages/manage/ManageBookingsPage'));
const AuditPage = lazy(() => import('@/pages/manage/AuditPage'));
const UsersPage = lazy(() => import('@/pages/admin/UsersPage'));
const NotFoundPage = lazy(() => import('@/pages/NotFoundPage'));

const router = createBrowserRouter([
  {
    element: <Layout />,
    children: [
      { index: true, element: <HotelsPage /> },
      { path: 'hotels/:id', element: <HotelPage /> },
      {
        path: 'login',
        element: (
          <GuestOnly>
            <LoginPage />
          </GuestOnly>
        ),
      },
      {
        path: 'register',
        element: (
          <GuestOnly>
            <RegisterPage />
          </GuestOnly>
        ),
      },
      {
        element: <RequireAuth />,
        children: [
          { path: 'profile', element: <ProfilePage /> },
          { path: 'notifications', element: <NotificationsPage /> },
          {
            element: <RequireRole roles={['customer']} />,
            children: [{ path: 'my/bookings', element: <MyBookingsPage /> }],
          },
          {
            element: <RequireRole roles={['hotel_manager', 'admin']} />,
            children: [
              { path: 'manage/hotels', element: <ManageHotelsPage /> },
              { path: 'manage/hotels/:id', element: <HotelEditPage /> },
              { path: 'manage/hotels/:id/room-types/new', element: <RoomTypeEditPage /> },
              { path: 'manage/room-types/:id', element: <RoomTypeEditPage /> },
              { path: 'manage/hotels/:id/audit', element: <AuditPage /> },
              { path: 'manage/bookings', element: <ManageBookingsPage /> },
            ],
          },
          {
            element: <RequireRole roles={['admin']} />,
            children: [
              { path: 'manage/hotels/new', element: <HotelEditPage /> },
              { path: 'admin/users', element: <UsersPage /> },
            ],
          },
        ],
      },
      { path: '*', element: <NotFoundPage /> },
    ],
  },
]);

export function App() {
  useSessionBootstrap();
  return <RouterProvider router={router} />;
}
