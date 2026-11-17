<script setup lang="ts">
import { computed, ref } from 'vue'
import { healthAPI, healthError, type HealthPreview } from '@/api/admin/account-health'

const props = defineProps<{ modelValue: string | null; inherited?: string; disabled?: boolean; global?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: string | null] }>()
const custom = ref(false)
const mode = computed(() => props.modelValue === null ? 'inherit' : props.modelValue === '' && !custom.value ? 'default' : 'custom')
const preview = ref<HealthPreview>()
const error = ref('')
const busy = ref(false)
const examples = '计算 {{rand:1-100}} + {{rand:1-100}}，只回复结果\n测活时间 {{time}}，请仅回复：OK\n当前日期 {{date}}，请仅回复：服务正常\n请求编号 {{rand}}，请原样返回该编号'
function setMode(event: Event) {
  const value = (event.target as HTMLSelectElement).value
  custom.value = value === 'custom'
  emit('update:modelValue', value === 'inherit' ? null : '')
  preview.value = undefined
}
async function render() {
  busy.value = true; error.value = ''
  try { preview.value = await healthAPI.preview(props.modelValue ?? props.inherited ?? '') }
  catch (e) { error.value = healthError(e) }
  finally { busy.value = false }
}
</script>

<template>
  <div class="space-y-2">
    <label class="block text-sm font-medium">测活内容</label>
    <select v-if="!global" :value="mode" class="input" :disabled="disabled" aria-label="测活内容来源" @change="setMode">
      <option value="inherit">使用继承内容</option><option value="default">默认 hi</option><option value="custom">自定义内容</option>
    </select>
    <textarea v-if="global || mode === 'custom'" :value="modelValue ?? ''" rows="4" class="input font-mono text-sm" :disabled="disabled" placeholder="留空使用 hi；多行时每次随机选择一行" aria-label="测活内容模板" @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)" />
    <p class="text-xs text-gray-500">每次随机抽取一行，留空为 hi；最大 16 KiB、100 行、每行及展开后 2 KiB。请勿填写私密信息。</p>
    <p v-pre class="text-xs text-gray-500">变量：{{time}}、{{date}}、{{datetime}}、{{timestamp}}、{{rand}}、{{rand:1-100}}。未知变量保留原文。</p>
    <div class="flex gap-2">
      <button type="button" class="btn btn-secondary btn-sm" :disabled="disabled" @click="custom = true; emit('update:modelValue', examples)">填入示例</button>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="disabled || busy" @click="render">{{ busy ? '渲染中…' : '仅本地预览' }}</button>
    </div>
    <div v-if="preview" class="rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-800">
      <p class="text-xs text-gray-500">时区 {{ preview.timezone }} · 候选 {{ preview.line }} · 下次真实测试的随机结果可能不同</p>
      <pre class="mt-1 whitespace-pre-wrap break-all">{{ preview.content }}</pre>
      <p v-for="warning in preview.warnings" :key="warning" class="mt-1 text-amber-700">{{ warning }}</p>
    </div>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
  </div>
</template>
