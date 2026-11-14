import { apiClient } from '../client'

export interface EgressOperation {
  operation_id: string
  kind: string
  target: string
  phase: string
  error?: string
  created_at: string
}
export interface EgressStatus {
  capability: string
  configured: boolean
  error?: string
  runtime: { enabled: boolean; revision: number; operation_id?: string }
  prefix: string
  node_id: string
  interface: string
  counts: Record<string, number>
  operations: EgressOperation[]
  supported_purposes: string[]
}
export interface EgressBinding {
  account_id: number
  parent_account_id?: number
  name: string
  owner_id: string
  current_mode: string
  target_mode: string
  policy_version: number
  binding_version: number
  source_ipv6?: string
  observed_ipv6?: string
  observed_at?: string
  binding_state?: string
  state: string
  require_ipv6: boolean
  schedulable: boolean
  account_status: string
  inherited: boolean
  ready: boolean
  last_error?: string
  can_enroll: boolean
  unsupported_reason?: string
  auto_enrollment?: 'queued' | 'running' | 'retry' | 'blocked' | 'completed' | 'cancelled'
  auto_retry_at?: string
  recovery_state?: 'retry' | 'running' | 'blocked' | 'completed' | 'cancelled'
  recovery_attempts?: number
  recovery_retry_at?: string
  recovered_at?: string
  recovery_error?: string
}
export interface EgressEvidence {
  id: number; account_id: number; occurred_at: string; request_id: string; connection_id: string
  purpose: string; protocol: string; event: string; source_ipv6: string; remote_address: string
  target_host: string; reused: boolean; policy_version: number; binding_version: number
  http_status: number; error_code: string
}
export interface EgressEvidencePage { items: EgressEvidence[]; dropped_since_start: number; persistence_unavailable: boolean; pending: number }
const base = '/admin/wayvia-egress'
export interface HistoricalBinding { binding_id: string; source_ipv6: string; binding_version: number; state: string }
export interface RetiredAccount { account_id: number; name: string; owner_id: string; policy_version: number; binding_id: string }
export const egressAPI = {
  evidence: async (id: number) => (await apiClient.get<EgressEvidencePage>(`${base}/accounts/${id}/evidence`, { params: { limit: 50 } })).data,
	retired: async () => (await apiClient.get<RetiredAccount[]>(`${base}/retired-accounts`)).data,
	history: async (id: number) => (await apiClient.get<HistoricalBinding[]>(`${base}/accounts/${id}/history`)).data,
	restore: async (id: number, version: number, action: 'restore-binding' | 'restore-account', bindingID: string, key: string) => (await apiClient.post<EgressOperation>(`${base}/accounts/${id}/${action}`, { expected_policy_version: version, binding_id: bindingID, idempotency_key: key, acknowledge_stream_interrupt: true })).data,
  status: async () => (await apiClient.get<EgressStatus>(`${base}/status`)).data,
  bindings: async (page = 1, q = '', state = '', pageSize = 20, accountID?: number) => (await apiClient.get<{ items: EgressBinding[]; total: number }>(`${base}/bindings`, { params: { page, page_size: pageSize, q, state, account_id: accountID } })).data,
  runtime: async (enabled: boolean, revision: number, key: string) => (await apiClient.put<EgressOperation>(`${base}/runtime`, { enabled, expected_revision: revision, idempotency_key: key, acknowledge_stream_interrupt: true })).data,
  change: async (row: EgressBinding, action: 'mode' | 'rotate' | 'probe', mode: string, key: string, enableScheduling = false) => {
    const body = { mode, expected_policy_version: row.policy_version, idempotency_key: key, acknowledge_stream_interrupt: true, acknowledge_legacy_egress: mode === 'legacy', enable_scheduling: enableScheduling }
    return (action === 'mode' ? await apiClient.put<EgressOperation>(`${base}/accounts/${row.account_id}/mode`, body) : await apiClient.post<EgressOperation>(`${base}/accounts/${row.account_id}/${action}`, body)).data
  },
  operation: async (id: string) => (await apiClient.get<EgressOperation>(`${base}/operations/${id}`)).data
}

export const egressStateLabels: Record<string, string> = {
  pending_enrollment: '待启用', ready: '已就绪', legacy: '原有出口', preparing: '核验中',
  pausing: '暂停中', paused: '已暂停', blocked: '不可用', credential_uncertain: '需重新授权', retired: '已停用'
}
export function egressStateLabel(row: EgressBinding): string {
  if (isRecoveryPending(row)) return row.recovery_state === 'running' ? '自动恢复中' : '等待自动恢复'
  if (row.current_mode !== 'strict_ipv6') {
    if (row.auto_enrollment === 'blocked') return '配置失败'
    if (row.auto_enrollment === 'retry') return '等待重试'
    if (row.auto_enrollment === 'running') return '自动配置中'
    if (row.auto_enrollment === 'queued') return '待自动配置'
  }
  return row.state === 'ready' && !row.ready ? '等待核验' : egressStateLabels[row.state] || row.state
}
export function isAutoEnrollmentPending(row: EgressBinding): boolean {
  return row.current_mode !== 'strict_ipv6' && ['queued', 'running', 'retry'].includes(row.auto_enrollment || '')
}
export function isRecoveryPending(row: EgressBinding): boolean {
  return row.current_mode === 'strict_ipv6' && ['blocked', 'preparing'].includes(row.state) && ['retry', 'running'].includes(row.recovery_state || '')
}
export function egressStateDetail(row: EgressBinding, enabled = true, configured = true): string {
  if (row.account_status !== 'active') return '账号未启用，调度保持暂停'
  if (row.unsupported_reason) return row.unsupported_reason
  if (row.state === 'credential_uncertain') return row.last_error || '凭据状态待确认，请更新授权'
  if (isRecoveryPending(row)) {
    if (!enabled) return '固定 IPv6 服务已暂停，自动恢复等待中'
    if (!row.schedulable) return '账号调度已暂停，自动恢复等待中'
    return row.recovery_state === 'running' ? '正在恢复原 IPv6 并核验，成功后恢复请求' : '暂时故障，等待自动重试；保留原 IPv6'
  }
  if (isAutoEnrollmentPending(row)) {
    if (!configured) return '等待固定 IPv6 服务配置，调度保持暂停'
    if (!enabled) return '固定 IPv6 服务已暂停，等待恢复'
    if (row.auto_enrollment === 'retry') return '配置暂未成功，将自动重试；调度保持暂停'
    return row.auto_enrollment === 'running' ? '正在配置并核验 IPv6，成功后自动开放调度' : '等待自动配置 IPv6，成功后自动开放调度'
  }
  if (row.last_error) return row.last_error
  if (!row.schedulable) return '调度已暂停'
  return row.ready ? '固定出口可用' : row.state === 'legacy' ? '尚未启用固定 IPv6' : '暂不接收业务请求'
}
export function egressStateClass(row: EgressBinding): string {
  if (row.ready) return 'badge-success'
  if (isRecoveryPending(row)) return 'badge-warning'
  if (row.auto_enrollment === 'retry') return 'badge-warning'
  if (row.auto_enrollment === 'blocked') return 'badge-danger'
  if (['blocked', 'credential_uncertain'].includes(row.state)) return 'badge-danger'
  if (['legacy', 'retired'].includes(row.state)) return 'badge-gray'
  return 'badge-warning'
}

// 同一轮账号列表渲染合并为一次按ID查询，避免同名匹配和每行请求。
let pending = new Map<number, { resolve: (row?: EgressBinding) => void; reject: (e: unknown) => void }[]>()
let flushTimer: ReturnType<typeof setTimeout> | undefined
export function loadAccountEgress(id: number): Promise<EgressBinding | undefined> {
  return new Promise((resolve, reject) => {
    const requests = pending.get(id) || []; requests.push({ resolve, reject }); pending.set(id, requests)
    if (!flushTimer) flushTimer = setTimeout(() => { flushTimer = undefined; void flushBindings() }, 0)
  })
}
async function flushBindings() {
  const batch = pending; pending = new Map()
  const ids = [...batch.keys()]
  for (let offset = 0; offset < ids.length; offset += 200) {
    const part = ids.slice(offset, offset + 200)
    try {
      const { data } = await apiClient.get<{ items: EgressBinding[] }>(`${base}/bindings`, { params: { account_ids: part.join(','), page_size: 200 } })
      const byID = new Map(data.items.map(row => [row.account_id, row]))
      for (const id of part) for (const item of batch.get(id) || []) item.resolve(byID.get(id))
    } catch (error) { for (const id of part) for (const item of batch.get(id) || []) item.reject(error) }
  }
}
export function egressError(error: unknown, fallback: string): string {
  return error && typeof error === 'object' && 'message' in error && typeof error.message === 'string' ? error.message : fallback
}
