<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { healthAPI, healthError, type HealthSelection, type HealthRun } from '@/api/admin/account-health'
import HealthContentInput from './HealthContentInput.vue'
import HealthRunList from './HealthRunList.vue'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'

const props = defineProps<{ show: boolean; selection: HealthSelection }>()
const emit = defineEmits<{ close: [] }>()
const frozen = ref<{ account_ids: number[]; count: number; concurrency: number; timeout_seconds: number }>()
const content = ref<string | null>(null)
const inherited = ref('')
const model = ref('')
const batchID = ref('')
const runs = ref<HealthRun[]>([])
const busy = ref(false)
const error = ref('')
const stepUp = useStepUp()
let timer: ReturnType<typeof setInterval> | undefined
let generation = 0
function stop() { if (timer) clearInterval(timer); timer = undefined }
async function refresh() {
  if (!batchID.value) return
  try { runs.value = await healthAPI.runs({ batch_id: batchID.value }) } catch (e) { error.value = healthError(e) }
}
async function start() {
  if (!frozen.value || busy.value) return
  const ids = [...frozen.value.account_ids]; busy.value = true; error.value = ''
  try {
    const result = await stepUp.run(() => healthAPI.batch(ids, model.value, content.value))
    batchID.value = result.batch_id; runs.value = result.runs.slice(0, 100)
    timer = setInterval(() => { if (!document.hidden) void refresh() }, 3000)
    await refresh()
  } catch (e) { if (!isStepUpCancelled(e)) error.value = healthError(e) }
  finally { busy.value = false }
}
async function older() {
  const last = runs.value.at(-1); if (!last) return
  try { stop(); runs.value = await healthAPI.runs({ batch_id: batchID.value, before: last.created_at, before_id: last.id }) } catch (e) { error.value = healthError(e) }
}
watch(() => props.show, async (show) => {
  stop(); const current = ++generation
  if (!show) return
  frozen.value = undefined; runs.value = []; batchID.value = ''; content.value = null; model.value = ''; error.value = ''; busy.value = true
  try {
    const [preview, config] = await Promise.all([healthAPI.batchPreview(props.selection), healthAPI.settings()])
    if (current !== generation) return
    frozen.value = preview; inherited.value = config.settings.test_content
  } catch (e) { if (current === generation) error.value = healthError(e) }
  finally { if (current === generation) busy.value = false }
})
onUnmounted(stop)
</script>

<template>
  <BaseDialog :show="show" title="批量连通性测试" width="wide" @close="emit('close')">
    <div class="space-y-4">
      <template v-if="!batchID">
        <p v-if="frozen" class="text-sm">已由服务器冻结 {{ frozen.count }} 个账号 · 总并发 {{ frozen.concurrency }} · 超时 {{ frozen.timeout_seconds }} 秒</p>
        <p class="text-xs text-gray-500">逐账号固定执行，自动跳过不支持的账号及重复凭据。本次操作不会创建或开启周期计划。</p>
        <label class="block text-sm">模型策略<input v-model="model" class="input mt-1" placeholder="留空按每个账号的生效配置选择；填写则统一指定" :disabled="busy" /></label>
        <HealthContentInput v-model="content" :inherited="inherited" :disabled="busy" />
        <p class="text-xs text-gray-500">继承时，各账号、平台或独立覆盖可能不同；预览仅展示全局示例，实际内容记录在逐账号结果中。</p>
        <button type="button" class="btn btn-primary" :disabled="busy || !frozen?.count" @click="start">{{ busy ? '处理中…' : `确认测试 ${frozen?.count || 0} 个账号` }}</button>
      </template>
      <template v-else>
        <p class="break-all text-xs text-gray-500">批次 {{ batchID }} · 关闭弹窗后后台继续执行。</p>
        <div class="flex gap-2"><button type="button" class="btn btn-secondary btn-sm" @click="refresh">最新结果</button><button type="button" class="btn btn-secondary btn-sm" :disabled="runs.length < 100" @click="older">更早记录</button></div>
        <HealthRunList :runs="runs" />
      </template>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    </div>
    <TotpStepUpDialog :controller="stepUp" />
  </BaseDialog>
</template>
