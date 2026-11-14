<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { egressAPI, egressError, type EgressEvidencePage } from '@/api/admin/wayvia-egress'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ accountId: number; source?: string; bindingVersion: number }>()
const data = ref<EgressEvidencePage>()
const loading = ref(false)
const error = ref('')
const businessOnly = ref(true)
let version = 0
let disposed = false
const events: Record<string, string> = {
  response_headers: '收到响应头', body_eof: '响应流结束', stream_failed: '响应流中断',
  request_failed: '请求连接失败', ws_handshake: 'WS 握手', ws_message_sent: 'WS 消息已发送',
  ws_send_failed: 'WS 发送失败', connection_unavailable: '未取得连接证据'
}
const purposes: Record<string, string> = { business: '业务', query: '账号查询', refresh: 'Token 刷新', prewarm: '预热', reauth: '重新授权' }
const items = computed(() => (data.value?.items || []).filter(item => !businessOnly.value || item.purpose === 'business' && item.event !== 'ws_handshake'))
async function load() {
  const current = ++version
  loading.value = true
  try {
    const result = await egressAPI.evidence(props.accountId)
    if (!disposed && current === version) { data.value = result; error.value = '' }
  } catch (e) {
    if (!disposed && current === version) error.value = egressError(e, '连接记录读取失败，请重试')
  } finally { if (!disposed && current === version) loading.value = false }
}
watch(() => props.accountId, () => { data.value = undefined; error.value = ''; void load() }, { immediate: true })
const timer = setInterval(() => { if (!document.hidden && !loading.value) void load() }, 5000)
onUnmounted(() => { disposed = true; ++version; clearInterval(timer) })
const time = (value: string) => new Date(value).toLocaleString()
</script>

<template>
  <section aria-labelledby="wayvia-evidence-title" class="border-t border-gray-100 pt-5 dark:border-dark-700">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h4 id="wayvia-evidence-title" class="font-medium text-gray-900 dark:text-white">真实连接证据</h4>
      <div class="flex items-center gap-3">
        <label class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300"><input v-model="businessOnly" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500">仅业务</label>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load"><Icon name="refresh" size="sm" class="mr-1.5" :class="{ 'animate-spin': loading }" />刷新记录</button>
      </div>
    </div>
    <p class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">来自真实请求使用的本机连接，包含复用连接。与上方回显核验分开记录；收到响应头、发送消息或响应流结束均不代表模型处理成功，也不是上游观测的公网 IP。</p>
    <p v-if="error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ error }}{{ data ? '；以下为上次读取的记录。' : '' }}</p>
    <p v-if="data?.persistence_unavailable || data?.dropped_since_start" role="status" class="mt-3 text-xs text-amber-700 dark:text-amber-300">{{ data.persistence_unavailable ? '记录保存暂不可用。' : '' }}{{ data.dropped_since_start ? `本次服务启动后有 ${data.dropped_since_start} 条记录未保存。` : '' }}记录可能不完整，不能用“没有记录”判断没有请求。</p>
    <p v-if="loading && !data" role="status" class="py-5 text-sm text-gray-500 dark:text-gray-400">正在读取连接记录…</p>
    <p v-else-if="!items.length && !error" class="py-5 text-sm text-gray-500 dark:text-gray-400">{{ businessOnly ? '暂无业务连接证据。发送一次真实业务请求后再查看；回显核验和 WS 握手不会显示在这里。' : '暂无连接记录。新版本开始记录后才会显示。' }}</p>
    <ul v-if="items.length" class="mt-3 max-h-96 divide-y divide-gray-100 overflow-y-auto dark:divide-dark-700">
      <li v-for="item in items" :key="item.id" class="space-y-2 py-3">
        <div class="flex flex-wrap items-center justify-between gap-2 text-xs">
          <span class="font-medium text-gray-900 dark:text-gray-100">{{ purposes[item.purpose] || item.purpose }} · {{ item.protocol }} · {{ events[item.event] || item.event }}<span v-if="item.http_status"> · HTTP {{ item.http_status }}</span></span>
          <time class="tabular-nums text-gray-500 dark:text-gray-400" :datetime="item.occurred_at">{{ time(item.occurred_at) }}</time>
        </div>
        <div class="flex flex-wrap items-center gap-2 text-xs">
          <code class="break-all">{{ item.source_ipv6 || '未取得实际源地址' }}</code>
          <span :class="['badge', item.source_ipv6 && item.source_ipv6 === source && item.binding_version === bindingVersion ? 'badge-success' : 'badge-gray']">{{ !item.source_ipv6 ? '无连接证据' : item.source_ipv6 === source && item.binding_version === bindingVersion ? '与当前绑定一致' : '非当前绑定证据' }}</span>
          <span class="text-gray-500 dark:text-gray-400">{{ item.reused ? '复用连接' : '首次使用' }} · 账号 #{{ item.account_id }}</span>
        </div>
        <p class="break-all text-xs text-gray-500 dark:text-gray-400">目标 {{ item.target_host }} · {{ item.remote_address || '未建连' }}</p>
        <p v-if="item.error_code" class="break-words text-xs text-red-600 dark:text-red-400">{{ item.error_code }}</p>
        <details class="text-xs text-gray-500 dark:text-gray-400">
          <summary class="w-fit cursor-pointer rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500">连接与策略编号</summary>
          <div class="mt-2 space-y-1 break-all"><div>请求 / 消息：<code>{{ item.request_id }}</code></div><div>连接：<code>{{ item.connection_id || '未取得' }}</code></div><div>策略 v{{ item.policy_version }} · 地址 v{{ item.binding_version }}</div></div>
        </details>
      </li>
    </ul>
    <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">显示最近 50 条中的{{ businessOnly ? '业务' : '全部' }}记录，每 5 秒更新。按出口身份滚动保留最多 200 条、最长 7 天，全站最多 50,000 条，每分钟清理；不是完整请求审计。</p>
  </section>
</template>
