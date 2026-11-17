<script setup lang="ts">
import { healthContentSources, healthErrorClasses, healthSources, healthStates, type HealthRun } from '@/api/admin/account-health'
defineProps<{ runs: HealthRun[] }>()
const date = (value: string | null) => value ? new Date(value).toLocaleString() : '—'
</script>

<template>
  <div class="space-y-2">
    <p v-if="!runs.length" class="text-sm text-gray-500">暂无检测记录。</p>
    <details v-for="run in runs" :key="run.id" class="rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
      <summary class="cursor-pointer">
        <span class="font-medium">#{{ run.account_id }} · {{ healthStates[run.state] || run.state }}</span>
        <span class="ml-2 text-gray-500">{{ healthSources[run.source] || run.source }} · {{ date(run.created_at) }}</span>
        <span v-if="run.error_class" class="ml-2 text-amber-700">{{ healthErrorClasses[run.error_class] || run.error_class }} {{ run.error_code }}</span>
      </summary>
      <dl class="mt-3 grid gap-1 text-xs">
        <div>模型 / 协议：{{ run.model || '未指定' }} / {{ run.protocol }}</div>
        <div>开始：{{ date(run.started_at) }}；完成：{{ date(run.finished_at) }}；耗时 {{ run.latency_ms }} ms</div>
        <div>内容来源：{{ healthContentSources[run.content_source] || '已有请求证据' }}；配置版本 {{ run.config_version }}</div>
        <div>HTTP：{{ run.http_status || '—' }}；状态变更：{{ run.state_changed ? '是' : '否' }}；重试 {{ run.attempt }}</div>
        <div v-if="run.state === 'queued'">下次执行：{{ date(run.not_before) }}</div>
        <div v-if="run.credential_account_id !== run.account_id">凭据账号：#{{ run.credential_account_id }}</div>
      </dl>
      <p v-if="run.reason" class="mt-2 break-all">{{ run.reason }}</p>
      <pre v-if="run.rendered_content" class="mt-2 whitespace-pre-wrap break-all rounded bg-gray-50 p-2 dark:bg-dark-800">{{ run.rendered_content }}</pre>
    </details>
  </div>
</template>
