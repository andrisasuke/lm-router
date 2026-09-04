export function errorText(error: unknown) {
  if (typeof error === 'string') return error
  if (error instanceof Error) return error.message
  return String(error)
}

export function formatDate(value: string) {
  if (!value) return '—'
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

export function shortEndpoint(value: string) {
  return value.replace(/^https?:\/\//, '')
}

export async function copyText(value: string) {
  await navigator.clipboard.writeText(value)
}
