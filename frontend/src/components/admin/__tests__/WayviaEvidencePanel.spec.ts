import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import WayviaEvidencePanel from '../WayviaEvidencePanel.vue'
import { egressAPI, type EgressEvidence, type EgressEvidencePage } from '@/api/admin/wayvia-egress'

vi.mock('@/api/admin/wayvia-egress', async () => {
  const actual = await vi.importActual<typeof import('@/api/admin/wayvia-egress')>('@/api/admin/wayvia-egress')
  return { ...actual, egressAPI: { evidence: vi.fn() } }
})
const source = '2606:4700:abcd:1234::42'
const event: EgressEvidence = { id: 1, account_id: 3, occurred_at: '2026-11-14T10:00:00Z', request_id: 'req-1', connection_id: 'conn-1', purpose: 'business', protocol: 'WS', event: 'ws_message_sent', source_ipv6: source, remote_address: '[2606:4700::1]:443', target_host: 'example.invalid', reused: true, policy_version: 3, binding_version: 2, http_status: 0, error_code: '' }
const page = (items: EgressEvidence[]): EgressEvidencePage => ({ items, dropped_since_start: 0, persistence_unavailable: false, pending: 0 })
let wrapper: VueWrapper | undefined
function render() { return wrapper = mount(WayviaEvidencePanel, { props: { accountId: 3, source, bindingVersion: 2 }, global: { stubs: { Icon: true } } }) }
beforeEach(() => { vi.mocked(egressAPI.evidence).mockResolvedValue(page([])) })
afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.clearAllMocks() })

describe('Wayvia actual connection evidence', () => {
  it('separates WS handshakes and refresh traffic from business evidence', async () => {
    vi.mocked(egressAPI.evidence).mockResolvedValue(page([event, { ...event, id: 2, event: 'ws_handshake' }, { ...event, id: 3, protocol: 'HTTP', purpose: 'refresh', event: 'response_headers' }]))
    const view = render(); await flushPromises()
    expect(view.findAll('li')).toHaveLength(1)
    expect(view.text()).toContain('WS 消息已发送')
    expect(view.text()).toContain('复用连接')
    expect(view.text()).toContain('与当前绑定一致')
    await view.get('input[type="checkbox"]').setValue(false)
    expect(view.findAll('li')).toHaveLength(3)
    expect(view.text()).toContain('Token 刷新')
  })
  it('does not claim proof for missing trace or an older binding', async () => {
    vi.mocked(egressAPI.evidence).mockResolvedValue(page([{ ...event, source_ipv6: '', event: 'connection_unavailable' }, { ...event, id: 2, binding_version: 1 }]))
    const view = render(); await flushPromises()
    expect(view.text()).toContain('未取得实际源地址')
    expect(view.text()).toContain('非当前绑定证据')
    expect(view.text()).not.toContain('与当前绑定一致')
  })
  it('shows persistence loss and rejects a late result for another account', async () => {
    let resolveOld!: (value: EgressEvidencePage) => void
    vi.mocked(egressAPI.evidence).mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve }))
    const view = render()
    vi.mocked(egressAPI.evidence).mockResolvedValue({ ...page([]), persistence_unavailable: true, dropped_since_start: 4 })
    await view.setProps({ accountId: 4 }); await flushPromises()
    resolveOld(page([event])); await flushPromises()
    expect(view.findAll('li')).toHaveLength(0)
    expect(view.text()).toContain('4 条记录未保存')
    expect(view.text()).toContain('记录保存暂不可用')
  })
})
