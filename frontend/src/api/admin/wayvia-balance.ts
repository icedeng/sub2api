import { apiClient } from '../client'

export interface UpstreamBalanceConfig {
  enabled: boolean
  provider: 'auto' | 'sub2api' | 'newapi'
  auto_refresh: boolean
  interval_minutes: number
  low_balance_threshold?: string
  query_user_id?: string
}

export interface UpstreamBalanceMetric {
  amount?: string
  unit?: string
  raw_amount?: string
  unlimited?: boolean
}

export interface UpstreamBalancePeriod {
  window: string
  remaining?: UpstreamBalanceMetric | null
  used?: UpstreamBalanceMetric | null
  limit?: UpstreamBalanceMetric | null
  resets_at?: string
  expires_at?: string
}

export interface UpstreamBalanceSnapshot {
  provider: string
  path: string
  wallet_path?: string
  wallet?: UpstreamBalanceMetric | null
  key_quota?: UpstreamBalanceMetric | null
  periods: UpstreamBalancePeriod[]
  today?: {
    requests?: string
    tokens?: string
    cost?: UpstreamBalanceMetric | null
    scope: string
  } | null
  received_at: string
  fresh_until: string
  wallet_unavailable_reason?: string
}

export interface UpstreamBalanceState {
  account_id: number
  eligible: boolean
  revision: number
  config: UpstreamBalanceConfig
  query_token_configured: boolean
  status: string
  error_code?: string
  last_attempt_at?: string
  next_retry_at?: string
  snapshot?: UpstreamBalanceSnapshot | null
}

export interface UpstreamBalanceUpdate {
  expected_revision: number
  config: UpstreamBalanceConfig
  query_token?: string
  clear_query_token?: boolean
}

const base = '/admin/wayvia-balance/accounts'

export const upstreamBalanceAPI = {
  get: async (id: number, signal?: AbortSignal): Promise<UpstreamBalanceState> =>
    (await apiClient.get<UpstreamBalanceState>(`${base}/${id}`, { signal })).data,
  list: async (ids: number[], signal?: AbortSignal): Promise<UpstreamBalanceState[]> =>
    (await apiClient.get<{ items: UpstreamBalanceState[] }>(base, {
      params: { ids: ids.join(',') }, signal
    })).data.items,
  update: async (id: number, input: UpstreamBalanceUpdate): Promise<UpstreamBalanceState> =>
    (await apiClient.put<UpstreamBalanceState>(`${base}/${id}`, input)).data,
  refresh: async (id: number, rediscover = false): Promise<UpstreamBalanceState> =>
    (await apiClient.post<UpstreamBalanceState>(`${base}/${id}/refresh`, { rediscover })).data
}

// 错误只映射本地文案，未知响应不直接回显上游内容或凭据。
export function upstreamBalanceErrorKey(code?: string): string {
  const known = new Set([
    'invalid_base_url', 'missing_api_key', 'proxy_unavailable', 'auth_failed',
    'rate_limited', 'upstream_unavailable', 'request_failed', 'response_too_large',
    'invalid_response', 'unsupported_response', 'upstream_business_error', 'metadata_unavailable',
    'query_token_invalid', 'query_user_mismatch', 'query_token_binding_changed',
    'coordination_unavailable', 'query_busy', 'refresh_in_progress', 'config_changed', 'disabled',
    'manual_cooldown', 'wallet_not_provided'
  ])
  return `admin.accounts.upstreamBalance.errors.${code && known.has(code) ? code : 'unknown'}`
}

export function upstreamBalanceWalletReasonKey(reason: string): string {
  const known = new Set([
    'not_provided', 'permission_required', 'subscription_only', 'key_quota_only',
    'query_token_invalid', 'query_user_mismatch', 'metadata_unavailable', 'query_token_binding_changed'
  ])
  return `admin.accounts.upstreamBalance.walletReasons.${known.has(reason) ? reason : 'not_provided'}`
}

// 十进制金额保持字符串，不经过 Number 或以本站货币格式化。
export function upstreamBalanceMetricText(
  metric: UpstreamBalanceMetric | null | undefined,
  labels: { missing: string; unlimited: string; raw: string }
): string {
  if (!metric) return labels.missing
  if (metric.unlimited) return labels.unlimited
  if (metric.amount !== undefined) return [metric.amount, metric.unit].filter(Boolean).join(' ')
  if (metric.raw_amount !== undefined) return `${metric.raw_amount} ${labels.raw}`
  return labels.missing
}

export function upstreamBalanceBelowThreshold(amount?: string, threshold?: string): boolean {
  if (!amount || !threshold) return false
  const decimal = /^(-?)(\d+)(?:\.(\d+))?$/
  const left = decimal.exec(amount)
  const right = decimal.exec(threshold)
  if (!left || !right || right[1]) return false
  if (left[1] && /[1-9]/.test(left[2] + (left[3] || ''))) return true
  const lhs = left[2].replace(/^0+(?=\d)/, '')
  const rhs = right[2].replace(/^0+(?=\d)/, '')
  if (lhs.length !== rhs.length) return lhs.length < rhs.length
  if (lhs !== rhs) return lhs < rhs
  const size = Math.max((left[3] || '').length, (right[3] || '').length)
  return (left[3] || '').padEnd(size, '0') < (right[3] || '').padEnd(size, '0')
}
