import { Box } from '@mui/material';
import { useState } from 'react';

// Обложка отеля: фото по ссылке или, если его нет, детерминированный
// «пейзаж» из id — у каждого отеля свои цвета, без внешних картинок.
const palettes: [string, string, string][] = [
  ['#0f5c63', '#3f8a90', '#f4c95d'],
  ['#1d3557', '#457b9d', '#f1faee'],
  ['#5f0f40', '#9a031e', '#fb8b24'],
  ['#264653', '#2a9d8f', '#e9c46a'],
  ['#3d405b', '#81b29a', '#f2cc8f'],
  ['#283618', '#606c38', '#dda15e'],
];

function hash(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}

export function HotelCover({
  seed,
  photo,
  height = 180,
  alt,
}: {
  seed: string;
  photo?: string;
  height?: number;
  alt: string;
}) {
  const [broken, setBroken] = useState(false);
  if (photo && !broken) {
    return (
      <Box
        component="img"
        src={photo}
        alt={alt}
        loading="lazy"
        onError={() => setBroken(true)}
        sx={{ width: '100%', height, objectFit: 'cover', display: 'block' }}
      />
    );
  }
  const [a, b, c] = palettes[hash(seed) % palettes.length]!;
  const sunX = 20 + (hash(seed + 'x') % 60);
  return (
    <Box
      role="img"
      aria-label={alt}
      sx={{ height, overflow: 'hidden', background: `linear-gradient(180deg, ${b} 0%, ${a} 100%)` }}
    >
      <svg viewBox="0 0 400 180" preserveAspectRatio="xMidYMid slice" width="100%" height="100%" aria-hidden>
        <circle cx={sunX * 4} cy="62" r="26" fill={c} opacity="0.9" />
        <path d="M0 140 Q 80 95 160 130 T 320 120 T 400 128 V180 H0Z" fill={a} opacity="0.55" />
        <path d="M0 160 Q 100 125 200 150 T 400 145 V180 H0Z" fill={a} />
        <g fill={c} opacity="0.85">
          <rect x="250" y="98" width="46" height="52" rx="2" />
          <rect x="300" y="84" width="34" height="66" rx="2" />
        </g>
        <g fill={a} opacity="0.6">
          {[0, 1, 2].map((r) =>
            [0, 1, 2].map((col) => (
              <rect key={`${r}-${col}`} x={256 + col * 13} y={106 + r * 13} width="7" height="7" />
            )),
          )}
        </g>
      </svg>
    </Box>
  );
}
