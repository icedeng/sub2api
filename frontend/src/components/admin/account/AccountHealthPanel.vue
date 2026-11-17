<script setup lang="ts">
import { ref, watch } from 'vue'
import { healthAPI, healthError, healthContentSources, type HealthEffective, type HealthPolicy, type HealthRun } from '@/api/admin/account-health'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import HealthContentInput from './HealthContentInput.vue'
import HealthRunList from './HealthRunList.vue'

const props = defineProps<{ accountId: number }>()
const effective = ref<HealthEffective>()
const policy = ref<HealthPolicy>({ mode: 'inherit', test_content: null })
const runs = ref<HealthRun[]>([])
const error = ref('')
const busy = ref(false)
const stepUp = useStepUp()
async function load() {
  const id = props.accountId; error.value = ''
  try {
    const [v, history] = await Promise.all([healthAPI.policy(id), healthAPI.runs({ account_id: id })])
    if (id !== props.accountId) return
    effective.value = v; policy.value = { ...v.policy, mode: v.policy.mode || 'inherit', test_content: v.policy.test_content ?? null }; runs.value = history
  } catch (e) { error.value = healthError(e) }
}
async function save() {
  busy.value = true; error.value = ''
  try { await stepUp.run(() => healthAPI.savePolicy(props.accountId, policy.value)); await load() }
  catch (e) { if (!isStepUpCancelled(e)) error.value = healthError(e) }
  finally { busy.value = false }
}
watch(() => props.accountId, () => { effective.value = undefined; runs.value = []; void load() }, { immediate: true })
</script>

<template>
  <section class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
    <div class="flex justify-between gap-3"><h3 class="font-semibold">账号健康策略与历史</h3><button type="button" class="btn btn-secondary btn-sm" @click="load">刷新</button></div>
    <template v-if="effective">
      <p class="text-sm">实际来源：{{ !effective.policy.mode ? '尚未纳入' : effective.policy.mode === 'exclude' ? '排除全局' : effective.policy.mode === 'override' ? '账号覆盖' : '继承全局' }} · {{ effective.independent_plans }} 个启用的独立计划</p>
      <p v-if="!effective.supported" class="text-sm text-amber-700">不支持该检测类型：首版仅支持普通 OpenAI OAuth / API Key 文本账号。</p>
      <p class="text-xs text-gray-500">{{ effective.model }} · {{ effective.protocol }} · {{ healthContentSources[effective.content_source] }} · 版本 {{ effective.version }} · {{ effective.timezone }}</p>
      <p class="text-xs text-gray-500">独立计划优先于全局周期；保存策略不会立即发送测试，人工暂停始终优先。</p>
      <label class="block text-sm">自动策略<select v-model="policy.mode" class="input mt-1" :disabled="busy"><option value="inherit">继承全局</option><option value="exclude">排除全局自动探测</option><option value="override">覆盖部分字段</option></select></label>
      <template v-if="policy.mode === 'override'">
        <label class="block text-sm">默认模型（空值继承）<input v-model="policy.model" class="input mt-1" :disabled="busy" /></label>
        <HealthContentInput v-model="policy.test_content" :inherited="effective.template" :disabled="busy" />
      </template>
      <button type="button" class="btn btn-primary btn-sm" :disabled="busy || !effective.supported" @click="save">保存账号策略</button>
    </template>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <p class="text-xs text-gray-500">文本检测成功仅代表该模型和 HTTP 协议；不表示 WS、额度查询或重置能力恢复。</p>
    <HealthRunList :runs="runs" />
    <TotpStepUpDialog :controller="stepUp" />
  </section>
</template>
