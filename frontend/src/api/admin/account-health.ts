import { apiClient } from '../client'

export interface HealthOverride { model?: string | null; test_content: string | null }
export interface HealthSettings {
  version: number; test_content: string; concurrency: number; timeout_seconds: number
  platforms: (HealthOverride & { platform: string; account_type: string })[]
  account_types: string[]; group_ids: number[]; error_recheck: boolean; periodic_enabled: boolean
  interval_minutes: number; skip_recent_success: boolean; cooldown_minutes: number
  auto_recover: boolean; retention_count: number; retention_days: number
}
export interface HealthPolicy extends HealthOverride { mode: '' | 'inherit' | 'exclude' | 'override' }
export interface HealthEffective {
  policy: HealthPolicy; supported: boolean; eligible: boolean; independent_plans: number
  model: string; protocol: string; template: string; content_source: string; version: number; timezone: string
}
export interface HealthRun {
  id: string; batch_id: string; account_id: number; credential_account_id: number; source: string
  config_version: number; model: string; protocol: string; content_source: string; rendered_content: string
  state: string; error_class: string; error_code: string; http_status: number; reason: string
  state_changed: boolean; attempt: number; created_at: string; started_at: string | null; finished_at: string | null
  latency_ms: number; not_before: string
}
export interface HealthSelection { account_ids?: number[]; filters?: Record<string, unknown> }
export interface HealthPreview { content: string; line: number; timezone: string; warnings: string[] }
export const healthSources: Record<string, string> = { manual: '手动测试', batch: '批量测试', scheduled: '独立计划', periodic: '全局周期', error_recheck: '错误复检', quota: '额度查询', quota_details: '重置卡详情查询', models: '模型目录', gateway: '业务请求' }
export const healthStates: Record<string, string> = { queued: '等待执行', running: '检测中', success: '成功', failed: '失败', skipped: '已跳过', cancelled: '已取消' }
export const healthContentSources: Record<string, string> = { global: '全局内容', platform: '平台 / 类型覆盖', account: '账号覆盖', scheduled: '独立计划覆盖', request: '本次自定义' }
const base = '/admin/account-health'
export const healthAPI = {
  settings: async () => (await apiClient.get<{ settings: HealthSettings; coverage: Record<string, number>; timezone: string }>(`${base}/settings`)).data,
  save: async (settings: HealthSettings) => (await apiClient.put<HealthSettings>(`${base}/settings`, settings)).data,
  preview: async (test_content: string) => (await apiClient.post<HealthPreview>(`${base}/preview`, { test_content })).data,
  policy: async (id: number) => (await apiClient.get<HealthEffective>(`/admin/accounts/${id}/health-policy`)).data,
  savePolicy: async (id: number, policy: HealthPolicy) => (await apiClient.put<HealthEffective>(`/admin/accounts/${id}/health-policy`, policy)).data,
  applyPreview: async (selection: HealthSelection) => (await apiClient.post<{ account_ids: number[]; count: number; preserved: number }>(`${base}/apply`, { ...selection, confirm: false })).data,
  apply: async (account_ids: number[]) => (await apiClient.post<{ applied: number }>(`${base}/apply`, { account_ids, confirm: true })).data,
  batchPreview: async (selection: HealthSelection) => (await apiClient.post<{ account_ids: number[]; count: number; concurrency: number; timeout_seconds: number }>(`${base}/batch-tests/preview`, selection)).data,
  batch: async (account_ids: number[], model: string, test_content: string | null) => (await apiClient.post<{ batch_id: string; runs: HealthRun[] }>(`${base}/batch-tests`, { account_ids, model, test_content })).data,
  runs: async (params: { account_id?: number; batch_id?: string; source?: string; before?: string; before_id?: string }) => (await apiClient.get<HealthRun[]>(`${base}/test-runs`, { params })).data,
  summaries: async (ids: number[]) => (await apiClient.get<HealthRun[]>(`${base}/summaries`, { params: { account_ids: ids.join(',') } })).data
}
export function healthError(error: unknown): string {
  return error && typeof error === 'object' && 'message' in error ? String(error.message) : '账号健康操作失败'
}

export const healthErrorClasses: Record<string, string> = { auth_revoked: '认证已撤销', auth: '认证异常', permission: '权限或边缘拦截', rate_limit: '额度 / 限流', network: '网络异常', upstream: '上游暂时异常', incomplete: '响应不完整', model: '模型请求失败', configuration: '配置问题', unsupported: '不支持的类型', cancelled: '已取消', internal: '执行异常', deduplicated: '重复任务已合并' }
