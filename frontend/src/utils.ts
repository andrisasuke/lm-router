export function errorText(error: unknown) {
  if (typeof error === 'string') return error
  if (error instanceof Error) return error.message
  return String(error)
}

export function formatDate(value: string) {
  if (!value) return '—'
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

export function formatQuotaReset(value: string, now = Date.now()) {
  if (!value) return '—'
  const reset = new Date(value).getTime()
  if (!Number.isFinite(reset)) return '—'

  const totalMinutes = Math.max(0, Math.ceil((reset - now) / 60_000))
  const days = Math.floor(totalMinutes / (24 * 60))
  const hours = Math.floor((totalMinutes % (24 * 60)) / 60)
  const minutes = totalMinutes % 60
  const parts: string[] = []
  if (days > 0) parts.push(`${days}d`)
  if (hours > 0) parts.push(`${hours}h`)
  parts.push(`${minutes}m`)
  return parts.join(' ')
}

export function shortEndpoint(value: string) {
  return value.replace(/^https?:\/\//, '')
}

export async function copyText(value: string) {
  await navigator.clipboard.writeText(value)
}
