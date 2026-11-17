<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { healthAPI, healthError, type HealthSettings } from '@/api/admin/account-health'
import HealthContentInput from '@/components/admin/account/HealthContentInput.vue'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'

const settings = ref<HealthSettings>()
const coverage = ref<Record<string, number>>({})
const timezone = ref('')
const groups = ref('')
const error = ref('')
const message = ref('')
const busy = ref(false)
const pendingIDs = ref<number[] | null>(null)
const preserved = ref(0)
const stepUp = useStepUp()
async function load() {
  error.value = ''
  try {
    const data = await healthAPI.settings()
    settings.value = data.settings; coverage.value = data.coverage; timezone.value = data.timezone; groups.value = data.settings.group_ids.join(', ')
    for (const type of ['oauth', 'apikey']) {
      if (!settings.value.platforms.some(p => p.account_type === type)) settings.value.platforms.push({ platform: 'openai', account_type: type, model: null, test_content: null })
    }
  } catch (e) { error.value = healthError(e) }
}
async function save() {
  if (!settings.value) return
  error.value = ''; message.value = ''; busy.value = true
  try {
    const ids = groups.value.trim() ? groups.value.split(',').map(s => Number(s.trim())) : []
    if (ids.some(id => !Number.isSafeInteger(id) || id <= 0)) throw new Error('分组 ID 需为逗号分隔的正整数')
    const payload = { ...settings.value, group_ids: [...new Set(ids)] }
    settings.value = await stepUp.run(() => healthAPI.save(payload))
    message.value = '配置已保存，仅影响后续适用测试；保存操作未发送模型请求。'
  } catch (e) { if (!isStepUpCancelled(e)) error.value = healthError(e) }
  finally { busy.value = false }
}
async function previewApply() {
  busy.value = true; error.value = ''
  try {
    const preview = await stepUp.run(() => healthAPI.applyPreview({ filters: {} }))
    pendingIDs.value = preview.account_ids; preserved.value = preview.preserved
  } catch (e) { if (!isStepUpCancelled(e)) error.value = healthError(e) }
  finally { busy.value = false }
}
async function apply() {
  if (!pendingIDs.value) return
  const ids = pendingIDs.value; pendingIDs.value = null; busy.value = true
  try { const result = await stepUp.run(() => healthAPI.apply(ids)); message.value = `已纳入 ${result.applied} 个账号。`; await load() }
  catch (e) { if (!isStepUpCancelled(e)) error.value = healthError(e) }
  finally { busy.value = false }
}
onMounted(load)
</script>

<template>
  <section class="card space-y-6 p-6">
    <div class="flex flex-wrap justify-between gap-3"><div><h2 class="text-lg font-semibold">账号健康与连通性测试</h2><p class="mt-1 text-sm text-gray-500">首版支持普通 OpenAI OAuth / API Key 文本账号。当前渲染时区：{{ timezone || '加载中' }}。</p></div><button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="load">重新加载</button></div>
    <template v-if="settings">
      <fieldset :disabled="busy" class="space-y-5">
        <legend class="mb-3 font-medium">连通性测试配置</legend>
        <HealthContentInput :model-value="settings.test_content" global :disabled="busy" @update:model-value="settings.test_content = $event ?? ''" />
        <div class="grid gap-4 sm:grid-cols-2">
          <label class="text-sm">检测总并发（所有测试入口共用）<input v-model.number="settings.concurrency" type="number" min="1" max="10" class="input mt-1" /></label>
          <label class="text-sm">单次超时（秒）<input v-model.number="settings.timeout_seconds" type="number" min="10" max="120" class="input mt-1" /></label>
        </div>
        <details v-for="platform in settings.platforms" :key="platform.account_type" class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
          <summary class="cursor-pointer text-sm font-medium">OpenAI {{ platform.account_type === 'oauth' ? 'OAuth' : 'API Key' }} 默认模型与内容</summary>
          <label class="mt-3 block text-sm">默认文本模型（留空沿用现有默认模型）<input v-model="platform.model" class="input my-2" placeholder="填写该类账号支持的文本模型 ID" /></label>
          <HealthContentInput v-model="platform.test_content" :inherited="settings.test_content" :disabled="busy" />
        </details>
      </fieldset>
      <fieldset :disabled="busy" class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
        <legend class="font-medium">自动健康策略</legend>
        <p class="text-xs text-gray-500">已有请求错误始终按统一规则分类。下面两个开关独立控制额外模型请求，初始均关闭。</p>
        <div class="flex flex-wrap gap-5 text-sm"><label><input v-model="settings.error_recheck" type="checkbox" class="mr-2" />错误后模型复检</label><label><input v-model="settings.periodic_enabled" type="checkbox" class="mr-2" />周期模型探测</label></div>
        <div class="flex flex-wrap gap-5 text-sm"><span>自动策略账号类型：</span><label><input v-model="settings.account_types" value="oauth" type="checkbox" class="mr-2" />OAuth</label><label><input v-model="settings.account_types" value="apikey" type="checkbox" class="mr-2" />API Key</label></div>
        <label class="block text-sm">适用分组 ID（逗号分隔，空值为全部分组）<input v-model="groups" class="input mt-1" placeholder="例如 1, 2" /></label>
        <div class="grid gap-4 sm:grid-cols-2"><label class="text-sm">周期间隔（分钟，最低 15）<input v-model.number="settings.interval_minutes" type="number" min="15" max="10080" class="input mt-1" /></label><label class="text-sm">错误复检冷却（分钟，最低 10）<input v-model.number="settings.cooldown_minutes" type="number" min="10" max="1440" class="input mt-1" /></label></div>
        <label class="block text-sm"><input v-model="settings.skip_recent_success" type="checkbox" class="mr-2" />同凭据、模型和协议近期有成功证据则跳过</label>
        <label class="block text-sm"><input v-model="settings.auto_recover" type="checkbox" class="mr-2" />成功后仅恢复本模块管理的临时状态</label>
        <p class="text-xs text-gray-500">不恢复人工暂停、不清除其他模型限流或额度查询故障。独立计划优先，业务并发和原账号出口仍生效。</p>
        <div class="grid gap-4 sm:grid-cols-2"><label class="text-sm">每账号最多保留条数<input v-model.number="settings.retention_count" type="number" min="1" max="100" class="input mt-1" /></label><label class="text-sm">最长保留天数<input v-model.number="settings.retention_days" type="number" min="1" max="30" class="input mt-1" /></label></div>
      </fieldset>
      <div class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
        <h3 class="font-medium">继承与覆盖情况</h3><p class="text-sm">继承 {{ coverage.inherit || 0 }} · 排除 {{ coverage.exclude || 0 }} · 覆盖 {{ coverage.override || 0 }} · 尚未纳入 {{ coverage.unmanaged || 0 }} · 有独立计划 {{ coverage.independent_plans || 0 }}</p>
        <p class="text-xs text-gray-500">新导入账号默认继承；已有账号需显式纳入。生效配置与检测历史可在账号列表的健康入口查看。</p>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="previewApply">预览应用于已有账号</button>
      </div>
      <div class="flex flex-wrap items-center gap-3"><button type="button" class="btn btn-primary" :disabled="busy" @click="save">{{ busy ? '处理中…' : '保存健康配置' }}</button><span class="text-xs text-gray-500">配置版本 {{ settings.version }}。保存内容不会自动开启探测，也不会触发一次批量测试。</span></div>
    </template>
    <p v-if="message" role="status" class="text-sm text-emerald-700">{{ message }}</p><p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <ConfirmDialog :show="pendingIDs !== null" title="纳入全局健康策略" :message="`将仅为 ${pendingIDs?.length || 0} 个尚未纳入的账号添加继承标记；保留 ${preserved} 个已有策略和所有独立计划、人工暂停。已开启的自动策略会在后续派发时生效。`" confirm-text="应用继承" @confirm="apply" @cancel="pendingIDs = null" />
    <TotpStepUpDialog :controller="stepUp" />
  </section>
</template>
