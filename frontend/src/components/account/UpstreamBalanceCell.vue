<script setup lang="ts">
import { computed, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { useStepUp, isStepUpBlocked, isStepUpCancelled } from '@/composables/useStepUp'
import { useAppStore } from '@/stores/app'
import { formatDateTime } from '@/utils/format'
import {
  upstreamBalanceAPI, upstreamBalanceErrorKey, upstreamBalanceWalletReasonKey, upstreamBalanceMetricText,
  upstreamBalanceBelowThreshold, type UpstreamBalanceMetric, type UpstreamBalanceState
} from '@/api/admin/wayvia-balance'

const props = defineProps<{
  account: { id: number; type: string }
  state?: UpstreamBalanceState
  loading?: boolean
  error?: boolean
  now: number
}>()
const emit = defineEmits<{ updated: [state: UpstreamBalanceState] }>()
const { t, te } = useI18n()
const appStore = useAppStore()
const stepUp = useStepUp()
const busy = ref(false)
let disposed = false
const snapshot = computed(() => props.state?.snapshot)
const stale = computed(() => Boolean(snapshot.value && (
  props.error || ['stale', 'config_changed', 'error', 'unavailable'].includes(props.state?.status || '') ||
  (props.state?.error_code && props.state.last_attempt_at &&
    Date.parse(snapshot.value.received_at) < Date.parse(props.state.last_attempt_at)) ||
  !Number.isFinite(Date.parse(snapshot.value.fresh_until)) || Date.parse(snapshot.value.fresh_until) <= props.now
)))
// next_retry_at 也用于普通定时调度，只有冷却状态才限制手动入口。
const cooldown = computed(() => props.state?.status === 'cooldown' && Boolean(
  props.state.next_retry_at && Date.parse(props.state.next_retry_at) > props.now
))
const status = computed(() => {
  if (props.error) return 'unavailable'
  if (!props.state) return props.loading ? 'loading' : 'unavailable'
  if (!props.state.config.enabled) return 'disabled'
  if (stale.value) return 'stale'
  return props.state.status
})
const statusLabel = computed(() => {
  const key = `admin.accounts.upstreamBalance.status.${status.value}`
  return te(key) ? t(key) : t('admin.accounts.upstreamBalance.status.unavailable')
})
const lowBalance = computed(() => Boolean(
  !stale.value && props.state?.config.enabled && snapshot.value?.wallet?.unit &&
  snapshot.value.wallet.unit !== 'quota' &&
  upstreamBalanceBelowThreshold(snapshot.value.wallet.amount, props.state.config.low_balance_threshold)
))
function metric(value?: UpstreamBalanceMetric | null) {
  return upstreamBalanceMetricText(value, {
    missing: t('admin.accounts.upstreamBalance.notProvided'),
    unlimited: t('admin.accounts.upstreamBalance.unlimited'),
    raw: t('admin.accounts.upstreamBalance.rawQuota')
  })
}
function date(value?: string) {
  return value && Number.isFinite(Date.parse(value)) ? formatDateTime(value) : '—'
}
async function refresh() {
  if (busy.value || !props.state?.config.enabled || cooldown.value) return
  busy.value = true
  const id = props.account.id
  try {
    const result = await stepUp.run(() => upstreamBalanceAPI.refresh(id))
    if (!disposed && id === props.account.id) emit('updated', result)
  } catch (error) {
    if (!disposed && !isStepUpCancelled(error)) {
      appStore.showError(t(isStepUpBlocked(error)
        ? 'admin.accounts.upstreamBalance.stepUpBlocked'
        : 'admin.accounts.upstreamBalance.refreshFailed'))
    }
  } finally {
    busy.value = false
  }
}
onUnmounted(() => { disposed = true; stepUp.onCancel() })
</script>

<template>
  <span v-if="account.type !== 'apikey' || state?.eligible === false" class="text-xs text-gray-400">
    {{ t('admin.accounts.upstreamBalance.notApplicable') }}
  </span>
  <div v-else class="min-w-[9rem] space-y-0.5 text-xs" data-testid="upstream-balance-cell">
    <template v-if="snapshot">
      <p :class="lowBalance ? 'font-medium text-amber-600 dark:text-amber-400' : 'text-gray-700 dark:text-gray-200'">
        {{ t('admin.accounts.upstreamBalance.wallet') }} {{ metric(snapshot.wallet) }}
      </p>
      <p class="text-gray-500 dark:text-gray-400">Key {{ metric(snapshot.key_quota) }}</p>
      <p v-if="lowBalance" class="text-amber-600 dark:text-amber-400">{{ t('admin.accounts.upstreamBalance.lowBalance') }}</p>
    </template>
    <div class="flex items-center gap-1">
      <HelpTooltip trigger="click" width-class="w-80 max-w-[calc(100vw-2rem)]">
        <template #trigger>
          <button type="button" class="border-b border-dotted border-gray-300 text-left text-xs" :class="stale ? 'text-amber-600 dark:text-amber-400' : 'text-gray-500 dark:text-gray-400'" :aria-label="t('admin.accounts.upstreamBalance.details')">
            {{ statusLabel }}<span v-if="snapshot?.periods?.length"> · {{ t('admin.accounts.upstreamBalance.periods') }}</span>
          </button>
        </template>
        <div class="max-h-80 space-y-1 overflow-y-auto break-words pr-3">
          <p class="font-medium">{{ t('admin.accounts.upstreamBalance.title') }}</p>
          <p>{{ statusLabel }}</p>
          <p v-if="state?.error_code">{{ t(upstreamBalanceErrorKey(state.error_code)) }}</p>
          <template v-if="snapshot">
            <p>{{ t('admin.accounts.upstreamBalance.wallet') }}: {{ metric(snapshot.wallet) }}</p>
            <p>Key: {{ metric(snapshot.key_quota) }}</p>
            <p v-if="snapshot.wallet_unavailable_reason">{{ t(upstreamBalanceWalletReasonKey(snapshot.wallet_unavailable_reason)) }}</p>
            <p>{{ t('admin.accounts.upstreamBalance.source') }}: {{ snapshot.provider }}</p>
            <p>{{ t('admin.accounts.upstreamBalance.path') }}: {{ snapshot.path }}</p>
            <p v-if="snapshot.wallet_path">{{ t('admin.accounts.upstreamBalance.walletPath') }}: {{ snapshot.wallet_path }}</p>
            <p>{{ t('admin.accounts.upstreamBalance.updatedAt') }}: {{ date(snapshot.received_at) }}</p>
            <div v-for="(period, index) in snapshot.periods" :key="`${period.window}-${index}`" class="border-t border-white/15 pt-1">
              <p>{{ period.window }} · {{ t('admin.accounts.upstreamBalance.remaining') }}: {{ metric(period.remaining) }}</p>
              <p>{{ t('admin.accounts.upstreamBalance.used') }}: {{ metric(period.used) }}</p>
              <p>{{ t('admin.accounts.upstreamBalance.limit') }}: {{ metric(period.limit) }}</p>
              <p v-if="period.resets_at">{{ t('admin.accounts.upstreamBalance.resetsAt') }}: {{ date(period.resets_at) }}</p>
              <p v-if="period.expires_at">{{ t('admin.accounts.upstreamBalance.expiresAt') }}: {{ date(period.expires_at) }}</p>
            </div>
            <div v-if="snapshot.today" class="border-t border-white/15 pt-1">
              <p>{{ t('admin.accounts.upstreamBalance.today') }} ({{ snapshot.today.scope === 'key' ? 'Key' : t('admin.accounts.upstreamBalance.userScope') }})</p>
              <p>{{ t('admin.accounts.upstreamBalance.requests') }}: {{ snapshot.today.requests ?? '—' }}</p>
              <p>Token: {{ snapshot.today.tokens ?? '—' }}</p>
              <p>{{ t('admin.accounts.upstreamBalance.cost') }}: {{ metric(snapshot.today.cost) }}</p>
            </div>
          </template>
          <p>{{ t('admin.accounts.upstreamBalance.lastAttempt') }}: {{ date(state?.last_attempt_at) }}</p>
          <p v-if="state?.next_retry_at">{{ t('admin.accounts.upstreamBalance.nextRetry') }}: {{ date(state.next_retry_at) }}</p>
          <p class="border-t border-white/15 pt-1">{{ t('admin.accounts.upstreamBalance.cacheHint') }}</p>
        </div>
      </HelpTooltip>
      <button type="button" class="inline-flex h-6 w-6 items-center justify-center rounded text-primary-600 hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-40 dark:text-primary-400 dark:hover:bg-dark-700" :disabled="busy || !state?.config.enabled || cooldown || state?.status === 'refreshing'" :aria-label="t('admin.accounts.upstreamBalance.refresh')" :title="t('admin.accounts.upstreamBalance.refresh')" @click.stop="refresh">
        <Icon name="refresh" size="xs" :class="{ 'animate-spin': busy }" />
      </button>
    </div>
    <p v-if="snapshot" class="text-[10px] text-gray-400 dark:text-dark-400">{{ date(snapshot.received_at) }}</p>
    <TotpStepUpDialog :controller="stepUp" />
  </div>
</template>
