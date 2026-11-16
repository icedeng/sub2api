import { onUnmounted, shallowRef, watch, type Ref } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { upstreamBalanceAPI, type UpstreamBalanceState } from '@/api/admin/wayvia-balance'

interface AccountIdentity { id: number; type: string; updated_at?: string }

// 账号列表按页读取余额缓存；不会随着每个单元格挂载访问上游。
export function useUpstreamBalanceCache(accounts: Ref<AccountIdentity[]>, enabled: Ref<boolean>) {
  const states = shallowRef<Record<number, UpstreamBalanceState>>({})
  const loading = shallowRef(false)
  const error = shallowRef(false)
  const now = shallowRef(Date.now())
  let controller: AbortController | null = null
  let requestGeneration = 0
  let identities = new Map<number, string>()

  function patch(state: UpstreamBalanceState) {
    if (!accounts.value.some(account => account.id === state.account_id)) return
    // 使更早开始的缓存读取失效，避免覆盖刚完成的手动刷新或配置保存。
    requestGeneration++
    controller?.abort()
    loading.value = false
    now.value = Date.now()
    states.value = { ...states.value, [state.account_id]: state }
  }

  async function reload() {
    controller?.abort()
    const generation = ++requestGeneration
    const ids = accounts.value.filter(account => account.type === 'apikey').map(account => account.id)
    if (!enabled.value || ids.length === 0) {
      states.value = {}
      loading.value = false
      error.value = false
      return
    }
    const activeIds = new Set(ids)
    states.value = Object.fromEntries(Object.entries(states.value).filter(([id]) => activeIds.has(Number(id))))
    controller = new AbortController()
    const signal = controller.signal
    loading.value = true
    error.value = false
    try {
      const items: UpstreamBalanceState[] = []
      for (let index = 0; index < ids.length; index += 100) {
        items.push(...await upstreamBalanceAPI.list(ids.slice(index, index + 100), signal))
      }
      if (generation !== requestGeneration) return
      states.value = Object.fromEntries(items.map(item => [item.account_id, item]))
    } catch {
      if (generation === requestGeneration && !signal.aborted) error.value = true
    } finally {
      if (generation === requestGeneration) loading.value = false
    }
  }

  watch(
    () => [enabled.value, accounts.value.map(account => `${account.id}:${account.type}:${account.updated_at || ''}`).join(',')],
    () => {
      // 账号更新后先撤下旧显示，避免新凭据下短暂展示上一身份的钱包。
      const current = new Map(accounts.value.map(account => [account.id, `${account.type}:${account.updated_at || ''}`]))
      states.value = Object.fromEntries(Object.entries(states.value).filter(([rawID]) => {
        const id = Number(rawID)
        return current.has(id) && current.get(id) === identities.get(id)
      }))
      identities = current
      void reload()
    },
    { immediate: true }
  )
  useIntervalFn(() => {
    if (enabled.value && !loading.value && !document.hidden) void reload()
  }, 60_000)
  // 全页共用时钟，冷却倒计时不为每个单元格创建计时器。
  useIntervalFn(() => { if (enabled.value) now.value = Date.now() }, 1000)
  onUnmounted(() => {
    requestGeneration++
    controller?.abort()
  })
  return { states, loading, error, now, reload, patch }
}
