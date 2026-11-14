import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'
import { createRouter, createMemoryHistory } from 'vue-router'
import WayviaEgressView from '../WayviaEgressView.vue'
import { apiClient } from '@/api/client'
import { egressAPI, type EgressBinding } from '@/api/admin/wayvia-egress'

vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn() }) }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ visible: ref(false), run: async (fn: () => Promise<unknown>) => fn() }),
  isStepUpCancelled: () => false
}))
vi.mock('@/api/admin/wayvia-egress', async () => {
  const actual = await vi.importActual<typeof import('@/api/admin/wayvia-egress')>('@/api/admin/wayvia-egress')
  return { ...actual, egressAPI: { ...actual.egressAPI, status: vi.fn(), bindings: vi.fn(), history: vi.fn(), retired: vi.fn(), evidence: vi.fn() } }
})

const TableStub = defineComponent({
  props: ['data', 'loading'],
  template: '<div><div v-for="row in data" :key="row.account_id"><slot name="cell-name" :row="row"/><slot name="cell-state" :row="row"/><slot name="cell-state_detail" :row="row"/><slot name="cell-actions" :row="row"/></div><slot v-if="!data.length" name="empty"/></div>'
})
const ConfirmStub = defineComponent({
  props: ['show', 'title', 'message', 'confirmText', 'danger'], emits: ['confirm', 'cancel'],
  template: '<div v-if="show" data-test="confirm"><p>{{ message }}</p><button data-test="confirm-submit" @click="$emit(\'confirm\')">{{ confirmText }}</button><button @click="$emit(\'cancel\')">取消</button></div>'
})
const DialogStub = defineComponent({ props: ['show'], template: '<div v-if="show"><slot/></div>' })
const pending: EgressBinding = { account_id: 10, name: 'new OAuth', owner_id: 'owner', current_mode: 'legacy', target_mode: 'legacy', policy_version: 1, binding_version: 0, state: 'pending_enrollment', require_ipv6: true, schedulable: false, account_status: 'active', inherited: false, ready: false, can_enroll: true }
let wrapper: VueWrapper | undefined
async function render(row = pending) {
  vi.mocked(egressAPI.bindings).mockResolvedValue({ items: [row], total: 1 })
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/', component: WayviaEgressView }] })
  await router.push('/')
  await router.isReady()
  wrapper = mount(WayviaEgressView, { global: { plugins: [router], stubs: {
    AppLayout: { template: '<div><slot/></div>' },
    TablePageLayout: { template: '<div><slot name="actions"/><slot name="filters"/><slot name="table"/><slot name="pagination"/></div>' },
    WayviaRuntimeControl: true, DataTable: TableStub, Pagination: true, Icon: true, Select: true,
    ConfirmDialog: ConfirmStub, BaseDialog: DialogStub, TotpStepUpDialog: true
  } } })
  await flushPromises()
  return wrapper
}
beforeEach(() => {
  vi.mocked(egressAPI.status).mockResolvedValue({ configured: true, capability: 'v1', runtime: { enabled: true, revision: 1 }, prefix: '', node_id: '', interface: '', counts: {}, operations: [], supported_purposes: [] })
  vi.mocked(egressAPI.history).mockResolvedValue([])
  vi.mocked(egressAPI.retired).mockResolvedValue([])
  vi.mocked(egressAPI.evidence).mockResolvedValue({items:[],dropped_since_start:0,persistence_unavailable:false,pending:0})
  vi.spyOn(apiClient, 'put').mockResolvedValue({ data: { operation_id: 'op', phase: 'accepted' } })
})
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.restoreAllMocks(); vi.clearAllMocks(); vi.useRealTimers() })

describe('Wayvia IPv6 activation flow', () => {
  it('shows automatic progress without requiring a manual activation click', async () => {
    const view = await render({ ...pending, auto_enrollment: 'queued' })
    expect(view.text()).toContain('待自动配置')
    expect(view.text()).toContain('成功后自动开放调度')
    expect(view.findAll('button').some(button => button.text() === '启用并开放调度')).toBe(false)
    expect(apiClient.put).not.toHaveBeenCalled()
  })

  it('shows retry progress while scheduling remains paused', async () => {
    const view = await render({ ...pending, state: 'blocked', auto_enrollment: 'retry', last_error: 'EGRESS_PROBE_FAILED: 回显连接失败' })
    expect(view.text()).toContain('等待重试')
    expect(view.text()).toContain('将自动重试；调度保持暂停')
    expect(view.findAll('button').some(button => button.text() === '启用并开放调度')).toBe(false)
  })

  it('shows the credential blocker without the contradictory activation instruction', async () => {
    const view = await render({ ...pending, auto_enrollment: 'blocked', can_enroll: false, unsupported_reason: '缺少 Access Token 和 Refresh Token，请补充账号凭据', last_error: 'EGRESS_ACCESS_TOKEN_MISSING' })
    expect(view.text()).toContain('配置失败')
    expect(view.text()).toContain('请补充账号凭据')
    expect(view.text()).not.toContain('账号已暂停，启用后可使用')
  })

  it('shows running recovery and its evidence entry without changing scheduling', async () => {
    const view = await render({ ...pending, current_mode: 'strict_ipv6', state: 'blocked', schedulable: true, recovery_state: 'retry', recovery_attempts: 2 })
    expect(view.text()).toContain('等待自动恢复')
    expect(view.text()).toContain('保留原 IPv6')
    await view.findAll('button').find(button => button.text() === '详情')!.trigger('click')
    await flushPromises()
    expect(view.text()).toContain('运行故障恢复')
    expect(view.text()).toContain('真实连接证据')
    expect(egressAPI.evidence).toHaveBeenCalledWith(10)
    expect(apiClient.put).not.toHaveBeenCalled()
  })

  it('requires confirmation and sends explicit scheduling consent for a new active account', async () => {
    const view = await render()
    expect(view.text()).toContain('待启用')
    expect(view.text()).not.toContain('纳管')
    expect(view.find('details').attributes('open')).toBeUndefined()
    expect(egressAPI.status).toHaveBeenCalledTimes(1)
    expect(egressAPI.retired).not.toHaveBeenCalled()
    await view.findAll('button').find(button => button.text() === '启用并开放调度')!.trigger('click')
    expect(apiClient.put).not.toHaveBeenCalled()
    expect(view.get('[data-test="confirm"]').text()).toContain('核验成功后同时开放此账号的调度')
    await view.get('[data-test="confirm-submit"]').trigger('click')
    await flushPromises()
    expect(apiClient.put).toHaveBeenCalledWith('/admin/wayvia-egress/accounts/10/mode', expect.objectContaining({ mode: 'strict_ipv6', enable_scheduling: true, expected_policy_version: 1 }))
  })

  it('does not enable scheduling for an administrator-disabled account', async () => {
    const view = await render({ ...pending, account_status: 'disabled' })
    await view.findAll('button').find(button => button.text() === '启用固定 IPv6')!.trigger('click')
    expect(view.get('[data-test="confirm"]').text()).toContain('不修改账号的调度开关')
    await view.get('[data-test="confirm-submit"]').trigger('click')
    await flushPromises()
    expect(apiClient.put).toHaveBeenCalledWith('/admin/wayvia-egress/accounts/10/mode', expect.objectContaining({ enable_scheduling: false }))
  })

  it('ignores a late response from an older search', async () => {
    vi.useFakeTimers()
    const view = await render()
    let resolveOld!: (value: { items: EgressBinding[]; total: number }) => void
    let resolveNew!: (value: { items: EgressBinding[]; total: number }) => void
    vi.mocked(egressAPI.bindings)
      .mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve }))
      .mockImplementationOnce(() => new Promise(resolve => { resolveNew = resolve }))
    await view.get('input[type="search"]').setValue('older')
    await vi.advanceTimersByTimeAsync(300)
    await view.get('input[type="search"]').setValue('latest')
    await vi.advanceTimersByTimeAsync(300)
    resolveNew({ items: [{ ...pending, name: 'latest result' }], total: 1 })
    await flushPromises()
    resolveOld({ items: [{ ...pending, name: 'outdated result' }], total: 1 })
    await flushPromises()
    expect(view.text()).toContain('latest result')
    expect(view.text()).not.toContain('outdated result')
  })
})
