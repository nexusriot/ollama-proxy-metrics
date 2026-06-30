// Shared formatting helpers.

export function fmtCost(n: number, currency = 'USD'): string {
  try {
    return new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency: currency || 'USD',
      maximumFractionDigits: n !== 0 && Math.abs(n) < 1 ? 4 : 2,
    }).format(n)
  } catch {
    return n.toFixed(4)
  }
}

export function fmtMs(ms: number): string {
  if (ms >= 60_000) return (ms / 60_000).toFixed(1) + 'm'
  if (ms >= 1_000)  return (ms / 1_000).toFixed(2) + 's'
  return Math.round(ms) + 'ms'
}

export function fmtNum(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000)     return (n / 1_000).toFixed(1) + 'K'
  return String(n)
}
