<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { egressError, egressAPI, type EgressStatus } from '@/api/admin/wayvia-egress'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'

// 管理页传入统一快照；系统设置页独立读取，避免双重轮询。
const props = defineProps<{ status?: EgressStatus; managed?: boolean; compact?: boolean; disabled?: boolean }>()
const emit = defineEmits<{ changed: []; busy: [value: boolean] }>()
const localStatus = ref<EgressStatus>()
const current = computed(() => props.managed ? props.status : localStatus.value)
const error = ref('')
const busy = ref(false)
const confirming = ref(false)
const requestedEnabled = ref(false)
const requestedRevision = ref(0)
const stepUp = useStepUp()
let timer: ReturnType<typeof setInterval> | undefined
async function load() {
  if (props.managed) return
  try { localStatus.value = await egressAPI.status(); error.value = '' }
  catch (e) { error.value = egressError(e, '出口配置加载失败') }
}
function confirmToggle() {
  if (!current.value || busy.value || props.disabled) return
  requestedEnabled.value = !current.value.runtime.enabled
  requestedRevision.value = current.value.runtime.revision
  confirming.value = true
}
async function toggle() {
  if (busy.value) return
  confirming.value = false
  busy.value = true
  emit('busy', true)
  error.value = ''
  const key = crypto.randomUUID()
  try {
    await stepUp.run(() => egressAPI.runtime(requestedEnabled.value, requestedRevision.value, key))
    await load()
    emit('changed')
  } catch (e) { if (!isStepUpCancelled(e)) error.value = egressError(e, '操作失败') }
  finally { busy.value = false; emit('busy', false) }
}
onMounted(() => {
  if (!props.managed) {
    void load()
    timer = setInterval(() => { if (!document.hidden && !busy.value) void load() }, 10000)
  }
})
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>

<template>
  <section :class="compact ? 'space-y-3' : 'card space-y-3 p-5'" aria-label="固定 IPv6 服务控制">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <h2 class="font-semibold text-gray-900 dark:text-white">固定 IPv6 出口</h2>
          <span class="badge" :class="!current ? 'badge-gray' : !current.configured ? 'badge-warning' : current.runtime.enabled ? 'badge-success' : 'badge-gray'">
            {{ !current ? '加载中' : !current.configured ? '待配置' : current.runtime.enabled ? '服务已开启' : '服务已暂停' }}
          </span>
        </div>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">新账号先暂停调度，系统自动配置并核验固定 IPv6，成功后自动开放使用。</p>
      </div>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="busy || disabled || !current?.configured" @click="confirmToggle">
        <Icon :name="current?.runtime.enabled ? 'ban' : 'play'" size="sm" class="mr-1.5" />
        {{ busy ? '提交中…' : current?.runtime.enabled ? '暂停固定出口服务' : '开启固定出口服务' }}
      </button>
    </div>
    <p v-if="current && !current.configured" class="text-sm text-amber-700 dark:text-amber-400">尚未完成服务器配置，请先配置宿主辅助服务、IPv6 地址池和共享持久目录。</p>
    <p v-if="error || current?.error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error || current?.error }}</p>
    <RouterLink v-if="!compact" class="inline-flex text-sm text-primary-600 hover:underline dark:text-primary-400" to="/admin/wayvia-egress">管理账号固定 IPv6 出口</RouterLink>
    <ConfirmDialog
      :show="confirming" :title="requestedEnabled ? '开启固定出口服务' : '暂停固定出口服务'"
      :message="requestedEnabled ? '将重新核验已启用账号的固定地址，并自动处理等待中的新账号；核验成功后才开放调度。' : '将暂停所有固定 IPv6 账号并中断现有请求，保留固定地址，不会切回普通出口。'"
      :confirm-text="requestedEnabled ? '开启服务' : '暂停服务'" :danger="!requestedEnabled"
      @confirm="toggle" @cancel="confirming = false"
    />
    <TotpStepUpDialog :controller="stepUp" />
  </section>
</template>
