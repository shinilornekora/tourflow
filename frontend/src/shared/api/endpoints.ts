// Эндпоинты всех сервисов. Каждый мутирующий запрос инвалидирует ровно те
// теги, которые он мог изменить, — RTK Query сам перезапросит открытые списки.
import { baseApi } from './baseApi';
import type {
  AuditRecord,
  Availability,
  Hotel,
  HotelDetails,
  HotelInput,
  HotelStatus,
  Notification,
  Paged,
  Reservation,
  ReservationStatus,
  Role,
  RoomStatus,
  RoomType,
  RoomTypeInput,
  Session,
  User,
} from './types';

export interface HotelQuery {
  q?: string;
  city?: string;
  stars_min?: number;
  guests?: number;
  price_max?: number;
  sort?: string;
  page?: number;
  page_size?: number;
  include_inactive?: boolean;
}

export interface ReservationQuery {
  hotel_id?: string;
  status?: ReservationStatus | '';
  from?: string;
  to?: string;
  page?: number;
  page_size?: number;
}

/** Убирает пустые параметры, чтобы URL и ключи кэша были стабильными. */
function clean<T extends object>(params: T): Record<string, string | number | boolean> {
  const out: Record<string, string | number | boolean> = {};
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '' && v !== null && v !== 0 && v !== false)
      out[k] = v as string | number | boolean;
  }
  return out;
}

export const api = baseApi.injectEndpoints({
  endpoints: (b) => ({
    // ---------- identity ----------
    login: b.mutation<Session, { email: string; password: string }>({
      query: (body) => ({ url: '/auth/login', method: 'POST', body }),
    }),
    register: b.mutation<Session, { email: string; password: string; full_name: string; phone: string }>({
      query: (body) => ({ url: '/auth/register', method: 'POST', body }),
    }),
    logout: b.mutation<void, void>({
      query: () => ({ url: '/auth/logout', method: 'POST' }),
    }),
    me: b.query<User, void>({ query: () => '/users/me', providesTags: ['Me'] }),
    updateMe: b.mutation<User, { full_name: string; phone: string; version: number }>({
      query: (body) => ({ url: '/users/me', method: 'PATCH', body }),
      invalidatesTags: ['Me'],
    }),
    changePassword: b.mutation<void, { current_password: string; new_password: string }>({
      query: (body) => ({ url: '/users/me/password', method: 'POST', body }),
    }),
    users: b.query<Paged<User>, { role?: Role | ''; q?: string; page?: number }>({
      query: (p) => ({ url: '/users', params: clean(p) }),
      providesTags: ['Users'],
    }),
    changeRole: b.mutation<User, { id: string; role: Role }>({
      query: ({ id, role }) => ({ url: `/users/${id}/role`, method: 'PATCH', body: { role } }),
      invalidatesTags: ['Users'],
    }),

    // ---------- catalog ----------
    hotels: b.query<Paged<Hotel>, HotelQuery>({
      query: (p) => ({ url: '/hotels', params: clean(p) }),
      providesTags: ['HotelList'],
    }),
    managedHotels: b.query<Paged<Hotel>, HotelQuery>({
      query: (p) => ({ url: '/hotels/managed', params: clean(p) }),
      providesTags: ['HotelList'],
    }),
    cities: b.query<{ items: string[] }, void>({ query: () => '/cities', providesTags: ['HotelList'] }),
    hotel: b.query<HotelDetails, string>({
      query: (id) => `/hotels/${id}`,
      providesTags: (_r, _e, id) => [{ type: 'Hotel', id }],
    }),
    createHotel: b.mutation<Hotel, HotelInput>({
      query: (body) => ({ url: '/hotels', method: 'POST', body }),
      invalidatesTags: ['HotelList'],
    }),
    updateHotel: b.mutation<Hotel, { id: string; body: HotelInput }>({
      query: ({ id, body }) => ({ url: `/hotels/${id}`, method: 'PUT', body }),
      invalidatesTags: (_r, _e, { id }) => [{ type: 'Hotel', id }, 'HotelList'],
    }),
    setHotelStatus: b.mutation<Hotel, { id: string; status: HotelStatus; version: number }>({
      query: ({ id, ...body }) => ({ url: `/hotels/${id}/status`, method: 'PATCH', body }),
      // Отключение отеля отменяет брони асинхронно — списки броней тоже устарели.
      invalidatesTags: (_r, _e, { id }) => [
        { type: 'Hotel', id },
        'HotelList',
        'Reservation',
        'Availability',
      ],
    }),
    assignManager: b.mutation<Hotel, { hotelId: string; userId: string }>({
      query: ({ hotelId, userId }) => ({
        url: `/hotels/${hotelId}/managers`,
        method: 'POST',
        body: { user_id: userId },
      }),
      invalidatesTags: (_r, _e, { hotelId }) => [{ type: 'Hotel', id: hotelId }],
    }),
    removeManager: b.mutation<Hotel, { hotelId: string; userId: string }>({
      query: ({ hotelId, userId }) => ({ url: `/hotels/${hotelId}/managers/${userId}`, method: 'DELETE' }),
      invalidatesTags: (_r, _e, { hotelId }) => [{ type: 'Hotel', id: hotelId }],
    }),
    roomType: b.query<{ room_type: RoomType; hotel: Hotel }, string>({
      query: (id) => `/room-types/${id}`,
      providesTags: (_r, _e, id) => [{ type: 'RoomType', id }],
    }),
    createRoomType: b.mutation<RoomType, { hotelId: string; body: RoomTypeInput }>({
      query: ({ hotelId, body }) => ({ url: `/hotels/${hotelId}/room-types`, method: 'POST', body }),
      invalidatesTags: (_r, _e, { hotelId }) => [{ type: 'Hotel', id: hotelId }, 'HotelList', 'Availability'],
    }),
    updateRoomType: b.mutation<RoomType, { id: string; hotelId: string; body: RoomTypeInput }>({
      query: ({ id, body }) => ({ url: `/room-types/${id}`, method: 'PUT', body }),
      invalidatesTags: (_r, _e, { id, hotelId }) => [
        { type: 'RoomType', id },
        { type: 'Hotel', id: hotelId },
        'HotelList',
        'Availability',
      ],
    }),
    setRoomStatus: b.mutation<RoomType, { id: string; hotelId: string; status: RoomStatus; version: number }>(
      {
        query: ({ id, status, version }) => ({
          url: `/room-types/${id}/status`,
          method: 'PATCH',
          body: { status, version },
        }),
        invalidatesTags: (_r, _e, { id, hotelId }) => [
          { type: 'RoomType', id },
          { type: 'Hotel', id: hotelId },
          'HotelList',
          'Availability',
        ],
      },
    ),

    // ---------- booking ----------
    availability: b.query<
      { items: Availability[] },
      { hotel_id: string; check_in: string; check_out: string; guests: number }
    >({
      query: (p) => ({ url: '/availability', params: p }),
      providesTags: ['Availability'],
      keepUnusedDataFor: 15, // остатки быстро устаревают
    }),
    book: b.mutation<
      Reservation,
      { key: string; room_type_id: string; check_in: string; check_out: string; guests: number }
    >({
      query: ({ key, ...body }) => ({
        url: '/reservations',
        method: 'POST',
        body,
        headers: { 'Idempotency-Key': key },
      }),
      invalidatesTags: ['Reservation', 'Availability', 'Notification'],
    }),
    myReservations: b.query<Paged<Reservation>, ReservationQuery>({
      query: (p) => ({ url: '/reservations/my', params: clean(p) }),
      providesTags: ['Reservation'],
    }),
    hotelReservations: b.query<Paged<Reservation>, ReservationQuery>({
      query: (p) => ({ url: '/reservations', params: clean(p) }),
      providesTags: ['Reservation'],
    }),
    reservation: b.query<Reservation, string>({
      query: (id) => `/reservations/${id}`,
      providesTags: (_r, _e, id) => [{ type: 'Reservation', id }],
    }),
    cancelReservation: b.mutation<Reservation, { id: string; reason?: string }>({
      query: ({ id, reason }) => ({
        url: `/reservations/${id}/cancel`,
        method: 'POST',
        body: { reason: reason ?? '' },
      }),
      invalidatesTags: ['Reservation', 'Availability', 'Notification', 'Audit'],
    }),
    audit: b.query<Paged<AuditRecord>, { hotel_id: string; page?: number }>({
      query: (p) => ({ url: '/audit', params: clean(p) }),
      providesTags: ['Audit'],
    }),

    // ---------- notification ----------
    notifications: b.query<Paged<Notification>, { page?: number; unread?: boolean }>({
      query: (p) => ({ url: '/notifications', params: clean(p) }),
      providesTags: ['Notification'],
    }),
    unreadCount: b.query<{ unread: number }, void>({
      query: () => '/notifications/unread-count',
      providesTags: ['Notification'],
    }),
    markRead: b.mutation<void, string>({
      query: (id) => ({ url: `/notifications/${id}/read`, method: 'POST' }),
      invalidatesTags: ['Notification'],
    }),
    markAllRead: b.mutation<{ marked: number }, void>({
      query: () => ({ url: '/notifications/read-all', method: 'POST' }),
      invalidatesTags: ['Notification'],
    }),
  }),
});

export const {
  useLoginMutation,
  useRegisterMutation,
  useLogoutMutation,
  useMeQuery,
  useUpdateMeMutation,
  useChangePasswordMutation,
  useUsersQuery,
  useChangeRoleMutation,
  useHotelsQuery,
  useManagedHotelsQuery,
  useCitiesQuery,
  useHotelQuery,
  useCreateHotelMutation,
  useUpdateHotelMutation,
  useSetHotelStatusMutation,
  useAssignManagerMutation,
  useRemoveManagerMutation,
  useRoomTypeQuery,
  useCreateRoomTypeMutation,
  useUpdateRoomTypeMutation,
  useSetRoomStatusMutation,
  useAvailabilityQuery,
  useBookMutation,
  useMyReservationsQuery,
  useHotelReservationsQuery,
  useReservationQuery,
  useCancelReservationMutation,
  useAuditQuery,
  useNotificationsQuery,
  useUnreadCountQuery,
  useMarkReadMutation,
  useMarkAllReadMutation,
} = api;
