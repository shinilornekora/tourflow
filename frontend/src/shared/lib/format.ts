import type { FetchBaseQueryError } from '@reduxjs/toolkit/query';
import type { SerializedError } from '@reduxjs/toolkit';
import type { ApiErrorBody, Money, ReservationStatus, Role } from '@/shared/api/types';

const rub = new Intl.NumberFormat('ru-RU', { style: 'currency', currency: 'RUB', maximumFractionDigits: 0 });

/** Деньги хранятся в копейках — форматируем без float-ошибок на вводе. */
export function formatMoney(m?: Money | null): string {
  if (!m) return '—';
  return rub.format(m.amount / 100);
}

const dateFmt = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric' });
const dateTimeFmt = new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' });

/** «2026-12-10» → «10 декабря 2026 г.» (без сдвига часового пояса). */
export function formatDate(iso: string): string {
  const [y, m, d] = iso.split('-').map(Number);
  return dateFmt.format(new Date(y ?? 1970, (m ?? 1) - 1, d ?? 1));
}

export function formatDateTime(iso: string): string {
  return dateTimeFmt.format(new Date(iso));
}

/** Локальная дата в формате ГГГГ-ММ-ДД со сдвигом на n дней. */
export function isoDay(offset = 0): string {
  const d = new Date();
  d.setDate(d.getDate() + offset);
  const mm = String(d.getMonth() + 1).padStart(2, '0');
  const dd = String(d.getDate()).padStart(2, '0');
  return `${d.getFullYear()}-${mm}-${dd}`;
}

export function nightsBetween(a: string, b: string): number {
  const ms = Date.parse(b) - Date.parse(a);
  return Number.isFinite(ms) ? Math.round(ms / 86_400_000) : 0;
}

export function plural(n: number, one: string, few: string, many: string): string {
  const m10 = n % 10;
  const m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
  return many;
}

export const roleLabel: Record<Role, string> = {
  customer: 'Клиент',
  hotel_manager: 'Менеджер отеля',
  admin: 'Администратор',
};

export const statusLabel: Record<ReservationStatus, string> = {
  confirmed: 'Подтверждена',
  cancelled: 'Отменена',
};

export interface ParsedError {
  status?: number | string;
  code: string;
  message: string;
  fields: Record<string, string>;
  requestId?: string;
}

/** Приводит ошибку RTK Query к единому виду для форм и алертов. */
export function parseError(
  err: FetchBaseQueryError | SerializedError | undefined | null,
): ParsedError | null {
  if (!err) return null;
  if ('status' in err) {
    const body = err.data as Partial<ApiErrorBody> | undefined;
    if (body?.error) {
      return {
        status: err.status,
        code: body.error.code,
        message: body.error.message,
        fields: body.error.fields ?? {},
        requestId: body.error.request_id,
      };
    }
    if (err.status === 'FETCH_ERROR') {
      return { status: err.status, code: 'network', message: 'Нет связи с сервером', fields: {} };
    }
    return {
      status: err.status,
      code: 'unknown',
      message: `Ошибка запроса (${String(err.status)})`,
      fields: {},
    };
  }
  return { code: 'client', message: err.message ?? 'Неизвестная ошибка', fields: {} };
}
