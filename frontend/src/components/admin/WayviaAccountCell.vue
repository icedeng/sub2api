<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { loadAccountEgress, egressStateLabel, egressStateClass, egressStateDetail, isAutoEnrollmentPending, isRecoveryPending, type EgressBinding } from '@/api/admin/wayvia-egress'
import { useClipboard } from '@/composables/useClipboard'

const props = defineProps<{ account: { id: number; name: string; platform: string; type: string; parent_account_id?: number | null } }>()
const binding = ref<EgressBinding>()
const unavailable = ref(false)
const { copyToClipboard } = useClipboard()
let request = 0
async function load() {
  const token = ++request
  if (props.account.platform !== 'openai' || props.account.type !== 'oauth') return
  try {
    const row = await loadAccountEgress(props.account.id)
    if (token === request) { binding.value = row; unavailable.value = false }
  } catch { if (token === request) unavailable.value = true }
}
watch(() => props.account, () => { binding.value = undefined; unavailable.value = false; void load() })
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  void load()
  timer = setInterval(() => {
    if (binding.value && (isAutoEnrollmentPending(binding.value) || isRecoveryPending(binding.value) || binding.value.state === 'preparing')) void load()
  }, 3000)
})
onUnmounted(() => { request++; if (timer) clearInterval(timer) })
</script>
<template>
  <span v-if="account.platform !== 'openai' || account.type !== 'oauth'" class="text-xs text-gray-500 dark:text-gray-400">原有出口</span>
  <div v-else class="max-w-48 space-y-1 text-xs">
    <span v-if="account.parent_account_id" class="text-gray-500 dark:text-gray-400">继承母账号 #{{ account.parent_account_id }}</span>
    <button v-if="binding?.source_ipv6" type="button" class="block max-w-full truncate font-mono text-primary-600 dark:text-primary-400" :title="'复制 ' + binding.source_ipv6" @click="copyToClipboard(binding.source_ipv6)">{{ binding.source_ipv6 }}</button>
    <RouterLink v-if="!unavailable" :to="{ path: '/admin/wayvia-egress', query: { account: account.parent_account_id || account.id } }" :class="binding ? ['badge', egressStateClass(binding)] : 'text-primary-600 dark:text-primary-400'">{{ binding ? egressStateLabel(binding) : '查看出口' }}</RouterLink>
    <p v-if="binding?.require_ipv6 && binding.current_mode !== 'strict_ipv6'" class="text-gray-500 dark:text-gray-400">{{ egressStateDetail(binding) }}</p>
    <span v-if="unavailable" class="text-gray-500 dark:text-gray-400">无查看权限或状态不可用</span>
  </div>
</template>
