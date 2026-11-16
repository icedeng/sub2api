<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, shallowRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { useStepUp, isStepUpBlocked, isStepUpCancelled } from '@/composables/useStepUp'
import { useAppStore } from '@/stores/app'
import { upstreamBalanceAPI, type UpstreamBalanceConfig, type UpstreamBalanceState } from '@/api/admin/wayvia-balance'

const props = defineProps<{ account: { id: number; type: string } }>()
const emit = defineEmits<{ updated: [state: UpstreamBalanceState] }>()
const { t } = useI18n()
const appStore = useAppStore()
const stepUp = useStepUp()
const state = shallowRef<UpstreamBalanceState | null>(null)
const open = ref(false)
const loading = ref(false)
const saving = ref(false)
const loadError = ref(false)
const conflict = ref(false)
const queryToken = ref('')
const clearQueryToken = ref(false)
let generation = 0
let controller: AbortController | null = null
let disposed = false
const form = reactive<UpstreamBalanceConfig>({ enabled: false, provider: 'sub2api', auto_refresh: true, interval_minutes: 5, low_balance_threshold: '', query_user_id: '' })
const supportsQueryToken = computed(() => form.provider === 'newapi' || form.provider === 'auto')

function apply(result: UpstreamBalanceState) {
  state.value = result
  Object.assign(form, { low_balance_threshold: '', query_user_id: '' }, result.config)
  queryToken.value = ''
  clearQueryToken.value = false
  conflict.value = false
  emit('updated', result)
}

async function load() {
  if (saving.value) return
  controller?.abort()
  const current = ++generation
  const id = props.account.id
  controller = new AbortController()
  loading.value = true
  loadError.value = false
  try {
    const result = await upstreamBalanceAPI.get(id, controller.signal)
    if (!disposed && current === generation) apply(result)
  } catch {
    if (!disposed && current === generation) loadError.value = true
  } finally {
    if (current === generation) loading.value = false
  }
}

function toggle(event: Event) {
  open.value = (event.target as HTMLDetailsElement).open
  if (open.value && !state.value && !loading.value) void load()
}

function preventAccountSubmit(event: KeyboardEvent) {
  // 此区域嵌在账号表单内，输入框回车不能误提交尚未保存的账号配置。
  if (event.target instanceof HTMLInputElement && event.target.type !== 'checkbox') event.preventDefault()
}

async function save() {
  if (!state.value || saving.value || loading.value || conflict.value) return
  const threshold = form.low_balance_threshold?.trim() || ''
  const userID = form.query_user_id?.trim() || ''
  if (!Number.isInteger(form.interval_minutes) || form.interval_minutes < 5 || form.interval_minutes > 1440 ||
      (threshold !== '' && !/^\d+(?:\.\d+)?$/.test(threshold)) ||
      (userID !== '' && !/^[1-9]\d*$/.test(userID)) ||
      (queryToken.value.trim() !== '' && (!supportsQueryToken.value || userID === ''))) {
    appStore.showError(t('admin.accounts.upstreamBalance.invalidSettings'))
    return
  }
  const id = props.account.id
  const current = generation
  const input = {
    expected_revision: state.value.revision,
    config: { ...form, low_balance_threshold: threshold, query_user_id: supportsQueryToken.value ? userID : '' },
    ...(queryToken.value.trim() && !clearQueryToken.value ? { query_token: queryToken.value.trim() } : {}),
    ...(clearQueryToken.value ? { clear_query_token: true } : {})
  }
  saving.value = true
  try {
    const result = await stepUp.run(() => upstreamBalanceAPI.update(id, input))
    if (disposed || current !== generation) return
    apply(result)
    appStore.showSuccess(t('admin.accounts.upstreamBalance.saved'))
  } catch (error) {
    if (disposed || current !== generation || isStepUpCancelled(error)) return
    if ((error as { status?: number }).status === 409) conflict.value = true
    appStore.showError(t(conflict.value
      ? 'admin.accounts.upstreamBalance.conflict'
      : isStepUpBlocked(error) ? 'admin.accounts.upstreamBalance.stepUpBlocked' : 'admin.accounts.upstreamBalance.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function rediscover() {
  if (saving.value || !state.value?.config.enabled) return
  saving.value = true
  const id = props.account.id
  const current = generation
  try {
    const result = await stepUp.run(() => upstreamBalanceAPI.refresh(id, true))
    if (!disposed && current === generation) {
      state.value = result
      emit('updated', result)
    }
  } catch (error) {
    if (!disposed && current === generation && !isStepUpCancelled(error)) {
      appStore.showError(t(isStepUpBlocked(error) ? 'admin.accounts.upstreamBalance.stepUpBlocked' : 'admin.accounts.upstreamBalance.refreshFailed'))
    }
  } finally {
    saving.value = false
  }
}

watch(() => props.account.id, () => {
  generation++
  controller?.abort()
  stepUp.onCancel()
  state.value = null
  queryToken.value = ''
  clearQueryToken.value = false
  conflict.value = false
  if (open.value) void load()
})
onUnmounted(() => {
  disposed = true
  generation++
  controller?.abort()
  stepUp.onCancel()
  queryToken.value = ''
})
</script>

<template>
  <details class="border-t border-gray-200 pt-4 dark:border-dark-600" data-testid="upstream-balance-settings" @toggle="toggle">
    <summary class="cursor-pointer text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.accounts.upstreamBalance.title') }}</summary>
    <div class="mt-3 space-y-3">
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.upstreamBalance.separateSaveHint') }}</p>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.upstreamBalance.savedAccountHint') }}</p>
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <p v-if="loadError" role="alert" class="text-sm text-red-600">{{ t('admin.accounts.upstreamBalance.loadFailed') }}</p>
      <p v-if="conflict" role="alert" class="text-sm text-amber-600">{{ t('admin.accounts.upstreamBalance.conflict') }}</p>
      <fieldset v-if="state?.eligible" :disabled="saving || loading || conflict" class="space-y-3 disabled:opacity-60" @keydown.enter="preventAccountSubmit">
        <div class="flex items-center justify-between gap-3">
          <label class="input-label mb-0">{{ t('admin.accounts.upstreamBalance.enable') }}</label>
          <Toggle v-model="form.enabled" :aria-label="t('admin.accounts.upstreamBalance.enable')" />
        </div>
        <div class="grid gap-3 sm:grid-cols-2">
          <label class="block">
            <span class="input-label">{{ t('admin.accounts.upstreamBalance.provider') }}</span>
            <select v-model="form.provider" class="input w-full"><option value="sub2api">Sub2API</option><option value="newapi">New API</option><option value="auto">{{ t('admin.accounts.upstreamBalance.autoDetect') }}</option></select>
          </label>
          <label class="block">
            <span class="input-label">{{ t('admin.accounts.upstreamBalance.interval') }}</span>
            <input v-model.number="form.interval_minutes" class="input w-full" type="number" min="5" max="1440" step="1" />
          </label>
        </div>
        <div class="flex items-center justify-between gap-3">
          <label class="input-label mb-0">{{ t('admin.accounts.upstreamBalance.autoRefresh') }}</label>
          <Toggle v-model="form.auto_refresh" :aria-label="t('admin.accounts.upstreamBalance.autoRefresh')" />
        </div>
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.upstreamBalance.threshold') }}</span>
          <input v-model="form.low_balance_threshold" class="input w-full" type="text" inputmode="decimal" maxlength="80" :placeholder="t('admin.accounts.upstreamBalance.optional')" />
          <span class="input-hint">{{ t('admin.accounts.upstreamBalance.thresholdHint') }}</span>
        </label>
        <details v-if="supportsQueryToken || state.query_token_configured" class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
          <summary class="cursor-pointer text-sm">{{ t('admin.accounts.upstreamBalance.advanced') }}</summary>
          <div class="mt-3 space-y-3">
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.upstreamBalance.tokenHint') }}</p>
            <p class="text-xs">{{ state.query_token_configured ? t('admin.accounts.upstreamBalance.tokenConfigured') : t('admin.accounts.upstreamBalance.tokenNotConfigured') }}</p>
            <label v-if="supportsQueryToken" class="block">
              <span class="input-label">{{ t('admin.accounts.upstreamBalance.userID') }}</span>
              <input v-model="form.query_user_id" class="input w-full" type="text" inputmode="numeric" maxlength="20" autocomplete="off" />
            </label>
            <label v-if="supportsQueryToken" class="block">
              <span class="input-label">{{ t('admin.accounts.upstreamBalance.queryToken') }}</span>
              <input v-model="queryToken" class="input w-full" type="password" autocomplete="new-password" maxlength="8192" :disabled="clearQueryToken" :placeholder="t('admin.accounts.upstreamBalance.keepToken')" />
            </label>
            <label v-if="state.query_token_configured" class="flex items-center gap-2 text-sm"><input v-model="clearQueryToken" type="checkbox" />{{ t('admin.accounts.upstreamBalance.clearToken') }}</label>
          </div>
        </details>
      </fieldset>
      <p v-else-if="state" class="text-sm text-gray-500">{{ t('admin.accounts.upstreamBalance.notApplicable') }}</p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-primary" :disabled="!state?.eligible || saving || loading || conflict" @click="save">{{ saving ? t('common.saving') : t('admin.accounts.upstreamBalance.save') }}</button>
        <button type="button" class="btn btn-secondary" :disabled="saving || loading" @click="load">{{ t('admin.accounts.upstreamBalance.reload') }}</button>
        <button v-if="state?.config.provider === 'auto'" type="button" class="btn btn-secondary" :disabled="saving || loading || !state.config.enabled" @click="rediscover">{{ t('admin.accounts.upstreamBalance.rediscover') }}</button>
      </div>
    </div>
    <Teleport to="body"><TotpStepUpDialog :controller="stepUp" /></Teleport>
  </details>
</template>
