export type Section = 'connections' | 'keys' | 'settings' | 'logs' | 'setup'
export type Provider = 'openai-codex' | 'anthropic-claude' | 'custom'

export interface ServerStatus {
  state: 'OFF' | 'STARTING' | 'ON' | 'ERROR'
  configuredEndpoint: string
  actualEndpoint: string
  host: string
  port: number
  error: string
}

export interface Connection {
  id: string
  provider: Provider
  name: string
  priority: number
  enabled: boolean
  status: 'Active' | 'Cooldown' | 'Needs re-auth' | 'Disabled'
  needsReauth: boolean
  routable: boolean
  cooldownUntil: string
  consecutiveFailures: number
  prefix: string
  baseUrl: string
  compatType: string
  apiType: string
  canQuota: boolean
  canRefresh: boolean
  canReauth: boolean
  requesting: boolean
}

export interface ConnectionActivity {
  id: string
  requesting: boolean
}

export interface APIKey {
  id: string
  name: string
  prefix: string
  createdAt: string
}

export interface CreatedKey extends APIKey {
  secret: string
}

export interface Settings {
  host: string
  port: number
  trayEnabled: boolean
  logRequests: boolean
  logUpstream: boolean
  logBodyLimit: number
  defaultModel: string
  upstreamTimeoutSeconds: number
}

export interface LogEntry {
  time: string
  source: 'app' | 'proxy' | 'openai'
  message: string
}

export interface TestResult {
  status: number
  ok: boolean
  output: string
}

export interface QuotaWindow {
  name: string
  utilization: number
  resetsAt: string
  summary: string
}

export interface QuotaResult {
  connected: boolean
  available: boolean
  status: number
  fetchedAt: string
  retryAt: string
  message: string
  windows: QuotaWindow[]
}

export interface OAuthSession {
  id: string
  provider: Provider
  accountId: string
  authUrl: string
  loopback: boolean
  loopbackError: string
}

export interface CustomProviderInput {
  name: string
  prefix: string
  baseUrl: string
  apiKey: string
  compatType: 'openai-compatible' | 'anthropic-compatible'
  apiType: 'chat' | 'responses' | ''
}
