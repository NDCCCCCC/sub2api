<template>
  <div
    v-if="visible"
    data-test="cn-provider-usage"
    class="min-w-[220px] space-y-1"
  >
    <!-- 额度窗口条（5h / weekly / monthly），来自探测结果或落库快照 -->
    <div v-if="tiers?.length" class="space-y-1">
      <UsageProgressBar
        v-for="tier in tiers"
        :key="tier.window"
        data-test="cn-provider-quota-tier"
        :label="windowLabel(tier.window)"
        :color="tier.window === 'weekly' ? 'emerald' : 'indigo'"
        :utilization="tier.used_percent"
        :resets-at="tier.reset_at"
      />
    </div>

    <!-- 余额行（static 展示：探测结果优先，其次落库快照） -->
    <div v-if="balanceVisible" class="flex flex-wrap items-center gap-1.5">
      <span
        data-test="cn-provider-balance-value"
        :class="['text-[10px] font-medium leading-4', platformTextClass(account.platform)]"
      >
        {{ balanceLabel }}
      </span>
      <span
        v-if="balanceLow"
        class="inline-flex items-center rounded bg-red-100 px-1 py-0.5 text-[10px] font-medium text-red-700 dark:bg-red-900/30 dark:text-red-300"
      >
        {{ t('admin.accounts.cnProviders.balanceLow') }}
      </span>
    </div>

    <!-- 单一查询入口：一次点击同时刷新额度窗口与余额（两者同属一个供应商账号，
         分两个按钮会让用户误以为查两次、并各自触发一次上游请求）。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="cn-provider-usage-probe"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.cnProviders.probeTooltip')"
        @click="handleProbe()"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.cnProviders.probe') }}
      </button>
    </div>

    <div
      v-if="error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type {
  CNProviderBalanceEntry,
  CNProviderBalanceResult,
  CNProviderQuotaProbeResult,
  CNQuotaTier
} from '@/api/admin/cnProviders'
import type { Account } from '@/types'
import { platformTextClass } from '@/utils/platformColors'
import UsageProgressBar from './UsageProgressBar.vue'
import { cnBalanceCellVisible, cnQuotaCellVisible } from './credentialsBuilder'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

// 额度窗口（coding plan / 订阅档）与余额（payg）对同一账号可能只有一侧适用：
// 合并单元格仍然只渲染适用的部分，但两者都适用时共用一个查询按钮。
const quotaVisible = computed(() => cnQuotaCellVisible(props.account))
const balanceVisible = computed(() => cnBalanceCellVisible(props.account))
const visible = computed(() => quotaVisible.value || balanceVisible.value)

const loading = ref(false)
const error = ref<string | null>(null)
const probeData = ref<CNProviderQuotaProbeResult | null>(null)
const probeBalance = ref<CNProviderBalanceResult | null>(null)
let probeGeneration = 0

const SNAPSHOT_STALE_MS = 15 * 60 * 1000
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const lastAutoProbeAt = new Map<number, number>()

const extraKey = (suffix: string) => `${props.account.platform}_${suffix}`

const readExtraNumber = (key: string): number | null => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}
const readExtraString = (key: string): string => {
  const v = (props.account.extra as Record<string, unknown> | undefined)?.[key]
  return typeof v === 'string' ? v : ''
}

// 落库的额度快照（缺三档键时返回 null，回落探测结果）。
const snapshotTiers = computed<CNQuotaTier[] | null>(() => {
  const platform = props.account.platform
  const used5h = readExtraNumber(`${platform}_5h_used_percent`)
  const usedWeekly = readExtraNumber(`${platform}_weekly_used_percent`)
  const usedMonthly = readExtraNumber(`${platform}_monthly_used_percent`)
  if (used5h == null && usedWeekly == null && usedMonthly == null) return null
  const tiers: CNQuotaTier[] = []
  if (used5h != null) tiers.push({ window: '5h', used_percent: used5h, reset_at: readExtraString(`${platform}_5h_reset_at`) || undefined })
  if (usedWeekly != null) tiers.push({ window: 'weekly', used_percent: usedWeekly, reset_at: readExtraString(`${platform}_weekly_reset_at`) || undefined })
  if (usedMonthly != null) tiers.push({ window: 'monthly', used_percent: usedMonthly, reset_at: readExtraString(`${platform}_monthly_reset_at`) || undefined })
  return tiers
})

const tiers = computed<CNQuotaTier[] | null>(() => {
  const probed = probeData.value
  if (probed?.success && probed.tiers?.length) return probed.tiers
  return snapshotTiers.value
})

// 落库的余额快照（多币种优先，其次单币种主值）。
const snapshotBalanceEntries = computed<CNProviderBalanceEntry[]>(() => {
  const multi = props.account.extra?.[extraKey('balances')]
  if (Array.isArray(multi)) {
    const entries = multi.flatMap((item): CNProviderBalanceEntry[] => {
      if (!item || typeof item !== 'object') return []
      const { currency, balance } = item as Record<string, unknown>
      if (typeof currency !== 'string' || typeof balance !== 'number') return []
      return [{ currency, balance }]
    })
    if (entries.length > 0) return entries
  }
  const single = readExtraNumber(extraKey('balance'))
  if (single != null) return [{ currency: readExtraString(extraKey('balance_currency')), balance: single }]
  return []
})

const balanceEntries = computed<CNProviderBalanceEntry[]>(() => {
  const probed = probeBalance.value
  if (probed?.success) {
    if (probed.balances?.length) return probed.balances
    return [{ currency: probed.currency || '', balance: probed.balance }]
  }
  return snapshotBalanceEntries.value
})

const formatEntry = (entry: CNProviderBalanceEntry): string => {
  const fixed = entry.balance >= 100 ? entry.balance.toFixed(0) : entry.balance.toFixed(2)
  return `${entry.currency || '¥'} ${fixed}`
}

const balanceLabel = computed(() => {
  if (balanceEntries.value.length === 0) return t('admin.accounts.cnProviders.balance')
  return balanceEntries.value.map(formatEntry).join(' · ')
})

const balanceLow = computed(() => props.account.extra?.[extraKey('balance_low')] === true)

const windowLabel = (window: string) => {
  if (window === 'weekly') return t('admin.accounts.cnProviders.windowWeekly')
  if (window === 'monthly') return t('admin.accounts.cnProviders.windowMonthly')
  return t('admin.accounts.cnProviders.window5h')
}

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

// 一次点击并行查询两个端点；任一失败只影响对应区块（保留旧快照展示 + 错误行）。
const handleProbe = async () => {
  if (loading.value) return
  const generation = ++probeGeneration
  const accountID = props.account.id
  const wantQuota = quotaVisible.value
  const wantBalance = balanceVisible.value
  loading.value = true
  error.value = null

  const results = await Promise.allSettled([
    wantQuota ? adminAPI.cnProviders.queryQuota(accountID) : Promise.resolve(null),
    wantBalance ? adminAPI.cnProviders.queryBalance(accountID) : Promise.resolve(null)
  ])
  if (generation !== probeGeneration) return

  const messages: string[] = []
  const [quotaRes, balanceRes] = results
  if (wantQuota) {
    if (quotaRes.status === 'fulfilled' && quotaRes.value) {
      if (quotaRes.value.success) probeData.value = quotaRes.value
      else messages.push(quotaRes.value.error || t('common.error'))
    } else if (quotaRes.status === 'rejected') {
      messages.push(extractErrorMessage(quotaRes.reason))
    }
  }
  if (wantBalance) {
    if (balanceRes.status === 'fulfilled' && balanceRes.value) {
      if (balanceRes.value.success) probeBalance.value = balanceRes.value
      else messages.push(balanceRes.value.error || t('common.error'))
    } else if (balanceRes.status === 'rejected') {
      messages.push(extractErrorMessage(balanceRes.reason))
    }
  }
  if (messages.length > 0) error.value = messages.join(' · ')
  loading.value = false
}

// 快照过期（无更新时间或超过 staleness 窗口）→ 挂载时自动探测一次（模块级去抖，
// 避免列表翻页/筛选重复挂载造成探测风暴）。
const snapshotIsStale = computed(() => {
  const updatedAt = readExtraString(`${props.account.platform}_usage_updated_at`) ||
    readExtraString(`${props.account.platform}_balance_updated_at`)
  if (!updatedAt) return true
  const ts = new Date(updatedAt).getTime()
  return Number.isNaN(ts) || Date.now() - ts > SNAPSHOT_STALE_MS
})

onMounted(() => {
  if (!visible.value) return
  if (!snapshotIsStale.value) return
  const last = lastAutoProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastAutoProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

// 列表刷新会用新的 extra 覆盖对象：仅当快照值或版本号真的变化时才丢弃手动探测结果。
watch(
  [snapshotTiers, snapshotBalanceEntries, () => readExtraString(`${props.account.platform}_usage_updated_at`)],
  (next, previous) => {
    if (JSON.stringify(next) !== JSON.stringify(previous)) {
      probeData.value = null
      probeBalance.value = null
    }
  }
)

watch(
  [() => props.account.id, () => props.account.platform],
  () => {
    probeGeneration++
    probeData.value = null
    probeBalance.value = null
    error.value = null
    loading.value = false
  }
)
</script>
