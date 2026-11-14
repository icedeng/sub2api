<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, shallowRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import WayviaEvidencePanel from '@/components/admin/WayviaEvidencePanel.vue'
import WayviaRuntimeControl from '@/components/admin/WayviaRuntimeControl.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import { useClipboard } from '@/composables/useClipboard'
import { useAppStore } from '@/stores/app'
import { egressError, egressAPI, egressStateLabels, egressStateLabel, egressStateClass, egressStateDetail, isAutoEnrollmentPending, isRecoveryPending, type EgressBinding, type EgressStatus, type HistoricalBinding, type RetiredAccount } from '@/api/admin/wayvia-egress'
import { apiClient } from '@/api/client'
import { getConfiguredTableDefaultPageSize } from '@/utils/tablePreferences'

const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const { copyToClipboard } = useClipboard()
const status = ref<EgressStatus>()
const rows = ref<EgressBinding[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(getConfiguredTableDefaultPageSize())
const query = ref('')
const stateFilter = ref('')
const error = ref('')
const loadError = ref('')
const loading = ref(true)
const refreshing = ref(false)
const busy = ref(false)
const runtimeBusy = ref(false)
const stepUp = useStepUp()
const selected = ref<EgressBinding>()
const history = ref<HistoricalBinding[]>([])
const historyLoading = ref(false)
const historyError = ref('')
const showOperations = ref(false)
const showRetired = ref(false)
const retired = ref<RetiredAccount[]>([])
const retiredLoading = ref(false)
const retiredError = ref('')
const auth = ref<{ auth_url: string; session_id: string; accountName: string }>()
const callback = ref('')
const authError = ref('')
const confirmation = shallowRef<{ title: string; message: string; confirmText: string; danger?: boolean; run: () => Promise<void> }>()
let timer: ReturnType<typeof setInterval> | undefined
let searchTimer: ReturnType<typeof setTimeout> | undefined
let loadVersion = 0
let detailVersion = 0
let disposed = false

const columns = [
  { key: 'account_id', label: 'ID' },
  { key: 'name', label: '账号' },
  { key: 'account_type', label: '账号类型' },
  { key: 'source_ipv6', label: '固定 IPv6' },
  { key: 'observed_at', label: '最近核验' },
  { key: 'state', label: '出口状态' },
  { key: 'state_detail', label: '状态说明' },
  { key: 'actions', label: '操作' }
]
const accountID = computed(() => {
  const id = Number(route.query.account)
  return Number.isSafeInteger(id) && id > 0 ? id : undefined
})
const activeOperation = computed(() => status.value?.operations.some(op => ['accepted', 'draining', 'preparing'].includes(op.phase)) ?? false)
const controlsBusy = computed(() => busy.value || runtimeBusy.value || activeOperation.value)
const stateOptions = computed(() => [
  { value: '', label: '全部状态' },
  ...Object.entries(egressStateLabels).map(([value, label]) => ({ value, label: label + (status.value?.counts[value] ? ' · ' + status.value.counts[value] : '') }))
])
const hasFilters = computed(() => !!query.value || !!stateFilter.value || !!accountID.value)
const phaseLabels: Record<string, string> = { accepted: '已提交', draining: '等待请求结束', preparing: '核验中', completed: '已完成', failed: '失败' }
const kindLabels: Record<string, string> = { mode: '变更出口', runtime: '全局服务', probe: '核验地址', rotate: '更换地址', 'restore-binding': '恢复地址', 'restore-account': '恢复账号', retire: '停用账号', recover: '自动恢复原地址' }
const bindingLabels: Record<string, string> = { pending: '待核验', active: '使用中', retained: '已保留' }

async function load(quiet = false) {
  const version = ++loadVersion
  refreshing.value = true
  if (!quiet) loading.value = true
  try {
    const [snapshot, list] = await Promise.all([
      egressAPI.status(),
      egressAPI.bindings(page.value, query.value.trim(), stateFilter.value, pageSize.value, accountID.value)
    ])
    if (disposed || version !== loadVersion) return
    status.value = snapshot
    rows.value = list.items
    total.value = list.total
    loadError.value = ''
    if (selected.value) {
      const fresh = rows.value.find(row => row.account_id === selected.value?.account_id)
      if (fresh) selected.value = fresh
    }
    const lastPage = Math.max(1, Math.ceil(list.total / pageSize.value))
    if (page.value > lastPage) page.value = lastPage
  } catch (e) {
    if (!disposed && version === loadVersion) loadError.value = egressError(e, '加载失败，请重试')
  } finally {
    if (!disposed && version === loadVersion) { loading.value = false; refreshing.value = false }
  }
}
function resetPage() {
  if (page.value === 1) void load()
  else page.value = 1
}
watch(query, () => {
  ++loadVersion // 输入后立即淘汰旧结果，避免慢请求覆盖新筛选。
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => { searchTimer = undefined; resetPage() }, 300)
})
watch([stateFilter, pageSize, accountID], resetPage)
watch(page, () => { void load() })
function searchNow() {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = undefined
  resetPage()
}
function clearFilters() {
  query.value = ''
  stateFilter.value = ''
  void router.replace({ query: { ...route.query, account: undefined } })
}
function strict(row: EgressBinding) { return row.current_mode === 'strict_ipv6' || row.target_mode === 'strict_ipv6' }
function primaryLabel(row: EgressBinding) {
  if (row.state === 'credential_uncertain') return '重新授权'
  if (!row.schedulable && row.account_status === 'active') return strict(row) ? '核验并开放调度' : '启用并开放调度'
  return strict(row) ? '重新核验' : '启用固定 IPv6'
}
function primaryDisabled(row: EgressBinding) {
  return controlsBusy.value || !status.value?.configured || row.state === 'retired' ||
    (row.state !== 'credential_uncertain' && (!row.can_enroll || !status.value?.runtime.enabled))
}
function primary(row: EgressBinding) {
  if (row.state === 'credential_uncertain') askReauthorize(row)
  else askChange(row, 'mode', 'strict_ipv6', !row.schedulable && row.account_status === 'active')
}
function askChange(row: EgressBinding, action: 'mode' | 'rotate' | 'probe', mode = 'strict_ipv6', enableScheduling = false) {
  const title = mode === 'legacy' ? '恢复原有出口' : action === 'rotate' ? '更换固定 IPv6' : action === 'probe' ? '核验固定地址' : enableScheduling ? primaryLabel(row) : strict(row) ? '重新核验固定 IPv6' : '启用固定 IPv6'
  const effect = mode === 'legacy'
    ? '将中断当前请求并恢复原有出口，旧 IPv6 地址保留。'
    : action === 'rotate'
      ? '将中断当前请求并分配新的固定 IPv6。仅在新地址核验通过后恢复，旧地址保留。'
      : '将中断当前请求，检查固定地址并执行外部 IPv6 回显核验；失败时保持阻断，不切回普通出口。'
  confirmation.value = {
    title, message: '账号「' + row.name + '」：' + effect + (enableScheduling ? '核验成功后同时开放此账号的调度；已禁用账号和影子账号的调度设置不变。' : '本次不修改账号的调度开关。'),
    confirmText: title, danger: mode === 'legacy' || action === 'rotate',
    run: async () => {
      const key = crypto.randomUUID()
      await stepUp.run(() => egressAPI.change(row, action, mode, key, enableScheduling))
      appStore.showSuccess('操作已提交，请查看出口状态与操作记录')
    }
  }
}
async function runConfirmed() {
  const action = confirmation.value
  if (!action || busy.value) return
  confirmation.value = undefined
  closeDetails()
  busy.value = true
  error.value = ''
  try { await action.run() }
  catch (e) { if (!isStepUpCancelled(e)) error.value = egressError(e, '操作失败，请刷新状态后重试') }
  finally { busy.value = false; await load(true) }
}
async function openDetails(row: EgressBinding) {
  const version = ++detailVersion
  selected.value = row
  history.value = []
  historyError.value = ''
  historyLoading.value = true
  try {
    const items = await egressAPI.history(row.parent_account_id || row.account_id)
    if (version === detailVersion && !disposed) history.value = items
  } catch (e) {
    if (version === detailVersion && !disposed) historyError.value = egressError(e, '读取历史地址失败')
  } finally { if (version === detailVersion) historyLoading.value = false }
}
function closeDetails() { selected.value = undefined; ++detailVersion }
function askRestore(id: number, version: number, action: 'restore-binding' | 'restore-account', bindingID = '') {
  const title = action === 'restore-account' ? '恢复原账号' : '恢复此地址'
  confirmation.value = {
    title, confirmText: title,
    message: action === 'restore-account' ? '恢复原账号及出口身份，保持禁用并要求重新授权。不会恢复已删除的影子账号或分组。' : '将中断当前请求并核验此账号原有的 IPv6 地址，不分配新地址；若凭据待确认，仍需重新授权。',
    run: async () => {
      const key = crypto.randomUUID()
      await stepUp.run(() => egressAPI.restore(id, version, action, bindingID, key))
      closeDetails()
      if (showRetired.value) await loadRetired()
      appStore.showSuccess('恢复操作已提交')
    }
  }
}
async function loadRetired() {
  showRetired.value = true
  retiredLoading.value = true
  retiredError.value = ''
  try { retired.value = await egressAPI.retired() }
  catch (e) { retiredError.value = egressError(e, '读取已删除账号失败') }
  finally { retiredLoading.value = false }
}
function askReauthorize(row: EgressBinding) {
  confirmation.value = {
    title: '重新授权 · ' + row.name, confirmText: '开始授权',
    message: '将中断当前连接。请使用原账号完成授权，凭据保存后仍需启用或核验固定 IPv6，业务不会提前放行。',
    run: async () => {
      const result = await stepUp.run(async () => (await apiClient.post<{ auth_url: string; session_id: string }>('/admin/wayvia-egress/reauth/generate-auth-url', { account_id: row.account_id })).data)
      closeDetails()
      auth.value = { ...result, accountName: row.name }
      callback.value = ''
      authError.value = ''
    }
  }
}
async function exchange() {
  if (!auth.value || busy.value) return
  busy.value = true
  authError.value = ''
  const sessionID = auth.value.session_id
  try {
    let url: URL
    try { url = new URL(callback.value.trim()) } catch { throw new Error('请粘贴浏览器中的完整回调地址') }
    const code = url.searchParams.get('code'), state = url.searchParams.get('state')
    if (!code || !state) throw new Error('回调地址缺少 code 或 state，请确认已完成授权')
    await stepUp.run(() => apiClient.post('/admin/wayvia-egress/reauth/exchange-code', { session_id: sessionID, code, state }, { timeout: 150000 }))
    auth.value = undefined
    callback.value = ''
    appStore.showSuccess('授权已保存，请继续启用或核验固定 IPv6')
    await load(true)
  } catch (e) { if (!isStepUpCancelled(e)) authError.value = egressError(e, '授权失败') }
  finally { busy.value = false }
}
function closeAuth() { if (!busy.value) { auth.value = undefined; callback.value = ''; authError.value = '' } }
function formatTime(value?: string) { return value ? new Date(value).toLocaleString() : '尚未核验' }
function observationExpired(row: EgressBinding) { return !!row.observed_at && Date.now() - Date.parse(row.observed_at) > 300000 }

onMounted(() => {
  void load()
  timer = setInterval(() => {
    if (!document.hidden && !refreshing.value && !busy.value && !runtimeBusy.value && !searchTimer) void load(true)
  }, 5000)
})
onUnmounted(() => {
  disposed = true
  ++loadVersion
  ++detailVersion
  if (timer) clearInterval(timer)
  if (searchTimer) clearTimeout(searchTimer)
})
</script>

<template>
  <AppLayout>
    <TablePageLayout>
      <template #actions>
        <div class="card p-4 sm:p-5">
          <WayviaRuntimeControl :status="status" managed compact :disabled="busy || activeOperation" @changed="load(true)" @busy="runtimeBusy = $event" />
          <details class="mt-3 border-t border-gray-100 pt-3 text-sm dark:border-dark-700">
            <summary class="w-fit cursor-pointer rounded text-gray-500 hover:text-gray-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 dark:text-gray-400 dark:hover:text-white">配置与支持范围</summary>
            <div class="mt-3 space-y-3">
              <dl class="grid gap-x-6 gap-y-2 sm:grid-cols-2 xl:grid-cols-4">
                <div><dt class="text-xs text-gray-500 dark:text-gray-400">节点</dt><dd class="mt-1 break-all">{{ status?.node_id || '未配置' }}</dd></div>
                <div><dt class="text-xs text-gray-500 dark:text-gray-400">IPv6 地址池</dt><dd class="mt-1 break-all font-mono text-xs">{{ status?.prefix || '未配置' }}</dd></div>
                <div><dt class="text-xs text-gray-500 dark:text-gray-400">网卡</dt><dd class="mt-1">{{ status?.interface || '未配置' }}</dd></div>
                <div><dt class="text-xs text-gray-500 dark:text-gray-400">能力版本</dt><dd class="mt-1">{{ status?.capability || '—' }}</dd></div>
              </dl>
              <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">支持普通 OpenAI OAuth，API Key 保持原有出口。已接入 {{ status?.supported_purposes?.join('、') || '—' }}；图片、搜索、Live 暂不支持固定出口。地址核验通过不代表所有上游业务均已验证。</p>
            </div>
          </details>
        </div>
      </template>
      <template #filters>
        <div class="space-y-3">
          <div v-if="error || loadError" role="alert" class="flex items-center justify-between gap-3 rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">
            <span>{{ error || loadError }}</span><button type="button" class="btn btn-secondary btn-sm shrink-0" :disabled="refreshing" @click="error = ''; load()">重试加载</button>
          </div>
          <div v-if="activeOperation" role="status" class="flex items-center gap-2 text-sm text-primary-700 dark:text-primary-300"><Icon name="refresh" size="sm" class="animate-spin motion-reduce:animate-none" />正在处理出口变更，完成后自动更新。核验期间相关账号暂停请求。</div>
          <div class="flex flex-wrap items-center gap-3">
            <div class="relative w-full sm:w-64">
              <Icon name="search" size="md" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
              <input v-model="query" type="search" class="input pl-10" placeholder="搜索账号名或 IPv6" aria-label="搜索账号名或 IPv6" @keyup.enter="searchNow">
            </div>
            <div class="w-full sm:w-40"><Select v-model="stateFilter" :options="stateOptions" aria-label="筛选出口状态" /></div>
            <button v-if="accountID" type="button" class="badge badge-primary gap-1.5" @click="router.replace({ query: { ...route.query, account: undefined } })" :aria-label="'清除账号 #' + accountID + ' 筛选'">账号 #{{ accountID }}<Icon name="x" size="sm" /></button>
            <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
              <button type="button" class="btn btn-secondary" aria-label="刷新出口状态" :disabled="refreshing" @click="load(true)"><Icon name="refresh" size="md" :class="{ 'animate-spin motion-reduce:animate-none': refreshing }" /></button>
              <button type="button" class="btn btn-secondary" @click="showOperations = true"><Icon name="clock" size="sm" class="mr-1.5" />操作记录</button>
              <button type="button" class="btn btn-secondary" @click="loadRetired">已删除账号</button>
            </div>
          </div>
        </div>
      </template>
      <template #table>
        <DataTable :columns="columns" :data="rows" :loading="loading" row-key="account_id">
          <template #cell-account_id="{ row }">
            <span class="tabular-nums text-gray-500 dark:text-gray-400">#{{ row.account_id }}</span>
          </template>
          <template #cell-name="{ row }">
            <span class="block max-w-56 truncate font-medium text-gray-900 dark:text-white" :title="row.name">{{ row.name }}</span>
          </template>
          <template #cell-account_type="{ row }">
            <span class="whitespace-nowrap text-gray-500 dark:text-gray-400">{{ row.inherited ? '影子账号 · 母账号 #' + row.parent_account_id : 'OpenAI OAuth' }}</span>
          </template>
          <template #cell-source_ipv6="{ row }">
            <button v-if="row.source_ipv6" type="button" class="inline-flex max-w-full items-center gap-2 rounded text-primary-600 hover:text-primary-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 dark:text-primary-400" :title="'复制 ' + row.source_ipv6" @click="copyToClipboard(row.source_ipv6)">
              <code class="break-all text-xs md:whitespace-nowrap md:break-normal">{{ row.source_ipv6 }}</code><Icon name="copy" size="sm" class="shrink-0" />
            </button>
            <span v-else class="text-gray-400 dark:text-gray-500">尚未分配</span>
          </template>
          <template #cell-observed_at="{ row }">
            <span class="whitespace-nowrap tabular-nums text-gray-500 dark:text-gray-400">{{ formatTime(row.observed_at) }}<span v-if="observationExpired(row)"> · 历史观测</span></span>
          </template>
          <template #cell-state="{ row }">
            <span :class="['badge whitespace-nowrap', egressStateClass(row)]">{{ egressStateLabel(row) }}</span>
          </template>
          <template #cell-state_detail="{ row }">
            <div class="flex flex-wrap items-center gap-2 md:flex-nowrap md:whitespace-nowrap">
              <span class="max-w-80 truncate text-gray-500 dark:text-gray-400" :title="egressStateDetail(row, status?.runtime.enabled, status?.configured)">{{ egressStateDetail(row, status?.runtime.enabled, status?.configured) }}</span>
              <button v-if="row.last_error" type="button" class="shrink-0 text-red-600 underline underline-offset-2 dark:text-red-400" @click="openDetails(row)">查看失败原因</button>
            </div>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex flex-nowrap items-center gap-2 whitespace-nowrap">
              <span v-if="isRecoveryPending(row) && row.schedulable" class="text-sm text-gray-500 dark:text-gray-400">自动恢复处理中</span>
              <span v-else-if="isAutoEnrollmentPending(row)" class="text-sm text-gray-500 dark:text-gray-400">{{ row.auto_enrollment === 'running' ? '自动处理中' : row.auto_enrollment === 'retry' ? '自动重试中' : '等待自动处理' }}</span>
              <button v-else-if="!row.inherited && (!row.ready || !row.schedulable)" type="button" class="btn btn-primary btn-sm" :disabled="primaryDisabled(row)" :title="!status?.runtime.enabled ? '请先开启固定出口服务' : row.unsupported_reason" @click="primary(row)">{{ primaryLabel(row) }}</button>
              <RouterLink v-else-if="row.inherited" :to="{ path: '/admin/wayvia-egress', query: { account: row.parent_account_id } }" class="btn btn-secondary btn-sm" @click="query = ''; stateFilter = ''">查看母账号</RouterLink>
              <button type="button" class="btn btn-secondary btn-sm" @click="openDetails(row)">详情</button>
            </div>
          </template>
          <template #empty>
            <Icon name="inbox" size="xl" class="mx-auto mb-3 text-gray-400" />
            <p class="font-medium text-gray-900 dark:text-gray-100">{{ loadError ? '暂时无法加载账号' : hasFilters ? '没有匹配的账号' : '还没有 OpenAI OAuth 账号' }}</p>
            <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ hasFilters ? '尝试调整搜索内容或清除筛选。' : '新建或导入账号后，会在这里等待启用固定 IPv6。' }}</p>
            <button v-if="hasFilters" type="button" class="btn btn-secondary btn-sm mt-4" @click="clearFilters">清除筛选</button>
            <RouterLink v-else class="btn btn-secondary btn-sm mt-4" to="/admin/accounts">前往账号管理</RouterLink>
          </template>
        </DataTable>
      </template>
      <template #pagination>
        <Pagination v-if="total > 0" :total="total" v-model:page="page" v-model:page-size="pageSize" />
        <p v-else class="text-sm text-gray-500 dark:text-gray-400">共 0 个账号</p>
      </template>
    </TablePageLayout>

    <BaseDialog :show="!!selected" :title="selected ? selected.name + ' · 出口详情' : '出口详情'" width="wide" @close="closeDetails">
      <div v-if="selected" class="space-y-5">
        <div class="flex flex-wrap items-center gap-2"><span :class="['badge', egressStateClass(selected)]">{{ egressStateLabel(selected) }}</span><span class="text-sm text-gray-500 dark:text-gray-400">账号 #{{ selected.account_id }}<span v-if="selected.require_ipv6"> · 必须使用固定 IPv6</span></span></div>
        <p v-if="selected.require_ipv6 && selected.current_mode !== 'strict_ipv6'" class="text-sm text-gray-600 dark:text-gray-300">{{ egressStateDetail(selected, status?.runtime.enabled, status?.configured) }}</p>
        <p v-if="selected.last_error || error" role="alert" class="break-words rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ error || selected.last_error }}</p>
        <dl class="grid gap-4 text-sm sm:grid-cols-2">
          <div><dt class="text-gray-500 dark:text-gray-400">固定地址</dt><dd class="mt-1 break-all font-mono">{{ selected.source_ipv6 || '尚未分配' }}</dd></div>
          <div><dt class="text-gray-500 dark:text-gray-400">回显地址</dt><dd class="mt-1 break-all font-mono">{{ selected.observed_ipv6 || '尚未核验' }}</dd></div>
          <div><dt class="text-gray-500 dark:text-gray-400">最近核验</dt><dd class="mt-1">{{ formatTime(selected.observed_at) }}</dd></div>
          <div><dt class="text-gray-500 dark:text-gray-400">策略 / 地址版本</dt><dd class="mt-1">v{{ selected.policy_version }} / v{{ selected.binding_version }}</dd></div>
        </dl>
        <div v-if="selected.recovery_state && selected.recovery_state !== 'cancelled'" class="space-y-1 text-sm">
          <h4 class="font-medium text-gray-900 dark:text-white">运行故障恢复</h4>
          <p class="text-gray-600 dark:text-gray-300">{{ isRecoveryPending(selected) ? egressStateDetail(selected, status?.runtime.enabled, status?.configured) : selected.recovery_state === 'blocked' ? '自动恢复已停止，请处理下方原因后重新核验。' : '最近一次恢复已完成。' }}</p>
          <p class="text-xs text-gray-500 dark:text-gray-400">已尝试 {{ selected.recovery_attempts || 0 }} 次<span v-if="selected.recovery_state === 'retry' && selected.schedulable && status?.runtime.enabled"> · 最早下次尝试 {{ formatTime(selected.recovery_retry_at) }}</span><span v-if="selected.recovered_at"> · 最近恢复 {{ formatTime(selected.recovered_at) }}</span>。只恢复原地址，不自动更换 IPv6 或打开手动暂停的调度。</p>
          <p v-if="selected.recovery_error" class="break-words text-xs text-amber-700 dark:text-amber-300">{{ selected.recovery_error }}</p>
        </div>
        <WayviaEvidencePanel :account-id="selected.account_id" :source="selected.source_ipv6" :binding-version="selected.binding_version" />
        <div v-if="!selected.inherited" class="flex flex-wrap gap-2 border-t border-gray-100 pt-4 dark:border-dark-700">
          <button v-if="!isAutoEnrollmentPending(selected)" type="button" class="btn btn-primary btn-sm" :disabled="primaryDisabled(selected)" @click="primary(selected)">{{ primaryLabel(selected) }}</button>
          <button v-if="selected.state !== 'credential_uncertain'" type="button" class="btn btn-secondary btn-sm" :disabled="controlsBusy || !status?.configured" @click="askReauthorize(selected)">重新授权</button>
          <template v-if="strict(selected)">
            <button type="button" class="btn btn-secondary btn-sm" :disabled="primaryDisabled(selected) || selected.state === 'credential_uncertain'" @click="askChange(selected, 'probe')">核验地址</button>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="primaryDisabled(selected) || selected.state === 'credential_uncertain'" @click="askChange(selected, 'rotate')">更换地址</button>
            <button v-if="!selected.require_ipv6" type="button" class="btn btn-ghost btn-sm text-red-600 dark:text-red-400" :disabled="controlsBusy || selected.state === 'credential_uncertain'" @click="askChange(selected, 'mode', 'legacy')">恢复原有出口</button>
          </template>
        </div>
        <div>
          <h4 class="font-medium text-gray-900 dark:text-white">历史地址</h4>
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">仅能恢复此账号原有地址。恢复地址不会解除“需重新授权”状态，也不会修改调度开关。</p>
          <p v-if="historyLoading" role="status" class="py-4 text-sm text-gray-500">正在读取…</p>
          <p v-else-if="historyError" role="alert" class="py-4 text-sm text-red-600 dark:text-red-400">{{ historyError }}</p>
          <p v-else-if="!history.length" class="py-4 text-sm text-gray-500 dark:text-gray-400">暂无历史地址</p>
          <ul v-else class="mt-3 divide-y divide-gray-100 dark:divide-dark-700">
            <li v-for="item in history" :key="item.binding_id" class="flex flex-wrap items-center justify-between gap-3 py-3">
              <div class="min-w-0"><code class="break-all text-xs">{{ item.source_ipv6 }}</code><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">v{{ item.binding_version }} · {{ bindingLabels[item.state] || item.state }}</p></div>
              <button v-if="!selected.inherited" type="button" class="btn btn-secondary btn-sm" :disabled="controlsBusy || !status?.configured || item.state === 'pending'" @click="askRestore(selected.account_id, selected.policy_version, 'restore-binding', item.binding_id)">恢复此地址</button>
            </li>
          </ul>
        </div>
      </div>
    </BaseDialog>

    <BaseDialog :show="showOperations" title="出口操作记录" width="wide" @close="showOperations = false">
      <p v-if="!status?.operations.length" class="py-8 text-center text-sm text-gray-500 dark:text-gray-400">暂无操作记录</p>
      <ul v-else class="divide-y divide-gray-100 dark:divide-dark-700">
        <li v-for="op in status.operations" :key="op.operation_id" class="space-y-2 py-4 first:pt-0">
          <div class="flex flex-wrap items-center justify-between gap-2"><span class="font-medium">{{ kindLabels[op.kind] || op.kind }}</span><span :class="['badge', op.phase === 'failed' ? 'badge-danger' : op.phase === 'completed' ? 'badge-success' : 'badge-primary']">{{ phaseLabels[op.phase] || op.phase }}</span></div>
          <p class="text-xs tabular-nums text-gray-500 dark:text-gray-400">{{ formatTime(op.created_at) }} · {{ op.target === 'strict_ipv6' ? '固定 IPv6' : op.target === 'legacy' ? '原有出口' : op.target === 'enabled' ? '开启服务' : op.target === 'paused' ? '暂停服务' : op.target }}</p>
          <p v-if="op.error" class="break-words text-sm text-red-600 dark:text-red-400">{{ op.error }}</p>
          <details class="text-xs text-gray-500 dark:text-gray-400"><summary class="cursor-pointer">操作编号</summary><code class="mt-1 block break-all">{{ op.operation_id }}</code></details>
        </li>
      </ul>
    </BaseDialog>

    <BaseDialog :show="showRetired && !stepUp.visible.value" title="已删除账号" width="wide" @close="showRetired = false">
      <p class="mb-4 text-sm text-gray-500 dark:text-gray-400">最近 200 个可恢复账号。恢复后保持禁用，需要重新授权和核验。</p>
      <p v-if="error || retiredError" role="alert" class="mb-3 text-sm text-red-600 dark:text-red-400">{{ error || retiredError }}</p>
      <p v-if="retiredLoading" role="status" class="py-6 text-center text-sm text-gray-500">正在读取…</p>
      <p v-else-if="!retired.length && !retiredError" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">没有可恢复的账号</p>
      <ul v-else class="divide-y divide-gray-100 dark:divide-dark-700">
        <li v-for="item in retired" :key="item.account_id" class="flex items-center justify-between gap-4 py-3">
          <div class="min-w-0"><p class="truncate font-medium">{{ item.name }}</p><p class="mt-1 text-xs text-gray-500 dark:text-gray-400">#{{ item.account_id }}</p></div>
          <button type="button" class="btn btn-secondary btn-sm shrink-0" :disabled="controlsBusy || !status?.configured" @click="askRestore(item.account_id, item.policy_version, 'restore-account')">恢复原账号</button>
        </li>
      </ul>
    </BaseDialog>

    <BaseDialog :show="!!auth && !stepUp.visible.value" :title="'重新授权 · ' + (auth?.accountName || '')" :close-on-escape="!busy" :show-close-button="!busy" @close="closeAuth">
      <form class="space-y-4" @submit.prevent="exchange">
        <p class="text-sm text-gray-600 dark:text-gray-300">使用原账号完成授权后，将浏览器中的完整回调地址粘贴到下方。保存后仍需启用或核验固定 IPv6。</p>
        <a v-if="auth" :href="auth.auth_url" target="_blank" rel="noopener noreferrer" class="btn btn-secondary">打开授权页面<Icon name="externalLink" size="sm" class="ml-2" /></a>
        <div><label for="wayvia-callback" class="input-label">完整回调地址</label><input id="wayvia-callback" v-model="callback" class="input" type="url" autocomplete="off" :disabled="busy" placeholder="http://localhost:1455/auth/callback?code=…&state=…" :aria-invalid="!!authError" aria-describedby="wayvia-auth-error"></div>
        <p id="wayvia-auth-error" v-if="authError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ authError }}</p>
        <div class="flex justify-end gap-3"><button type="button" class="btn btn-secondary" :disabled="busy" @click="closeAuth">取消</button><button type="submit" class="btn btn-primary" :disabled="busy || !callback.trim()">{{ busy ? '保存中…' : '保存授权' }}</button></div>
      </form>
    </BaseDialog>
    <ConfirmDialog :show="!!confirmation" :title="confirmation?.title || ''" :message="confirmation?.message || ''" :confirm-text="confirmation?.confirmText" :danger="confirmation?.danger" @confirm="runConfirmed" @cancel="confirmation = undefined" />
    <TotpStepUpDialog :controller="stepUp" />
  </AppLayout>
</template>
