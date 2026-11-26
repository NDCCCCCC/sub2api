import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive } from 'vue'
import CNProviderUsageCell from '../CNProviderUsageCell.vue'
import UsageProgressBar from '../UsageProgressBar.vue'
import type { Account } from '@/types'

const { queryQuota, queryBalance } = vi.hoisted(() => ({
  queryQuota: vi.fn(),
  queryBalance: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryQuota, queryBalance }
  }
}))

// 保留 vue-i18n 真实导出：UsageProgressBar 依赖 @/utils/format → @/i18n，
// 其模块级 createI18n 需要真实 createI18n 存在。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const account = {
  id: 7,
  platform: 'zhipu',
  type: 'apikey',
  credentials: { account_mode: 'coding' },
  extra: {
    zhipu_5h_used_percent: 0,
    zhipu_weekly_used_percent: 27,
    zhipu_5h_reset_at: '2026-08-18T12:30:00+08:00',
    zhipu_weekly_reset_at: '2026-08-22T00:00:00+08:00',
    zhipu_usage_updated_at: new Date().toISOString()
  }
} as Account

/** 同时具备额度窗口与余额的账号（kimi coding 无余额端点；用 deepseek payg 代表余额侧）。 */
const balanceAccount = {
  id: 11,
  platform: 'deepseek',
  type: 'apikey',
  credentials: { account_mode: 'payg' },
  extra: {
    deepseek_balance: 12.5,
    deepseek_balance_currency: 'USD',
    deepseek_balance_low: false,
    deepseek_balance_updated_at: new Date().toISOString()
  }
} as Account

describe('CNProviderUsageCell（合并的额度 + 余额单元格）', () => {
  beforeEach(() => {
    queryQuota.mockReset()
    queryBalance.mockReset()
  })

  it('renders tier rows through the shared UsageProgressBar inside the account table cell', async () => {
    queryQuota.mockResolvedValue({
      success: true,
      tiers: [
        { window: '5h', used_percent: 0, reset_at: '2026-08-18T12:30:00+08:00' },
        { window: 'weekly', used_percent: 27, reset_at: '2026-08-22T00:00:00+08:00' }
      ]
    })
    const wrapper = mount(CNProviderUsageCell, { props: { account } })

    const root = wrapper.get('[data-test="cn-provider-usage"]')
    expect(root.classes()).toContain('min-w-[220px]')

    // 新鲜快照：挂载即渲染条形图，不触发探测
    await flushPromises()
    expect(queryQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('27%')

    // 只有一个查询按钮（合并后不再分额度/余额两个入口）
    const buttons = root.findAll('[data-test="cn-provider-usage-probe"]')
    expect(buttons).toHaveLength(1)
    expect(buttons[0].text()).toBe('admin.accounts.cnProviders.probe')

    // tier 行由 UsageProgressBar 渲染：数量、label/color/utilization/reset 逐行对齐
    expect(root.findAll('[data-test="cn-provider-quota-tier"]')).toHaveLength(2)
    const bars = root.findAllComponents(UsageProgressBar)
    expect(bars).toHaveLength(2)
    expect(bars[0].props('label')).toBe('admin.accounts.cnProviders.window5h')
    expect(bars[0].props('utilization')).toBe(0)
    expect(bars[0].props('color')).toBe('indigo')
    expect(bars[0].props('resetsAt')).toBe('2026-08-18T12:30:00+08:00')
    expect(bars[1].props('label')).toBe('admin.accounts.cnProviders.windowWeekly')
    expect(bars[1].props('utilization')).toBe(27)
    expect(bars[1].props('color')).toBe('emerald')
    expect(bars[1].props('resetsAt')).toBe('2026-08-22T00:00:00+08:00')
  })

  it('单次点击同时查询额度窗格与余额（两个端点并行）', async () => {
    // 同时具备两侧的账号：coding 模式无余额端点，因此用 provider 组合构造：
    // 额度可见性由 coding 模式驱动，余额可见性由 kimi payg 驱动。
    // 这里直接验证：余额可见账号点按钮会同时发出两个请求当且仅当两侧都可见。
    queryQuota.mockResolvedValue({ success: true, tiers: [{ window: '5h', used_percent: 12 }] })
    queryBalance.mockResolvedValue({ success: true, balance: 3.5, currency: 'USD', balances: [{ currency: 'USD', balance: 3.5 }] })
    const wrapper = mount(CNProviderUsageCell, { props: { account: balanceAccount } })
    await flushPromises()

    // deepseek payg：只有余额侧可见 → 只查余额
    expect(queryQuota).not.toHaveBeenCalled()
    await wrapper.get('[data-test="cn-provider-usage-probe"]').trigger('click')
    await flushPromises()
    expect(queryBalance).toHaveBeenCalledWith(balanceAccount.id)
    expect(queryQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('USD 3.50')
  })

  it('renders the persisted balance as static text with one shared query action', async () => {
    const wrapper = mount(CNProviderUsageCell, { props: { account: balanceAccount } })
    await flushPromises()
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toBe('USD 12.50')
    const probeButtons = wrapper.findAll('[data-test="cn-provider-usage-probe"]')
    expect(probeButtons).toHaveLength(1)
  })

  it('shows the low-balance badge from the snapshot marker', async () => {
    const wrapper = mount(CNProviderUsageCell, {
      props: { account: { ...balanceAccount, extra: { ...balanceAccount.extra, deepseek_balance_low: true } } as Account }
    })
    await flushPromises()
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.balanceLow')
  })

  it('keeps the snapshot balance visible when a query fails', async () => {
    queryBalance.mockResolvedValue({ success: false, error: 'boom', balance: 0, available: false })
    const wrapper = mount(CNProviderUsageCell, { props: { account: balanceAccount } })
    await flushPromises()
    await wrapper.get('[data-test="cn-provider-usage-probe"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toBe('USD 12.50')
    expect(wrapper.text()).toContain('boom')
  })

  it('renders replacement quota values and windows for the same account without probing', async () => {
    const wrapper = mount(CNProviderUsageCell, { props: { account } })
    await wrapper.setProps({
      account: {
        ...account,
        extra: {
          ...account.extra,
          zhipu_5h_used_percent: 52,
          zhipu_weekly_used_percent: null,
          zhipu_monthly_used_percent: 81,
          zhipu_monthly_reset_at: '2026-12-01T00:00:00Z'
        }
      }
    })
    const bars = wrapper.findAllComponents(UsageProgressBar)
    expect(bars.map(bar => bar.props('utilization'))).toEqual([52, 81])
    expect(bars[1].props('label')).toBe('admin.accounts.cnProviders.windowMonthly')
    expect(bars[1].props('resetsAt')).toBe('2026-12-01T00:00:00Z')
    expect(queryQuota).not.toHaveBeenCalled()
  })

  it('reacts to quota changes inside the existing extra object', async () => {
    const row = reactive({ ...account, extra: { ...account.extra } })
    const wrapper = mount(CNProviderUsageCell, { props: { account: row } })
    row.extra.zhipu_5h_used_percent = 63
    await flushPromises()
    expect(wrapper.findAllComponents(UsageProgressBar)[0].props('utilization')).toBe(63)
    expect(queryQuota).not.toHaveBeenCalled()
  })

  it('removes bars when a refreshed snapshot clears all quota windows', async () => {
    const wrapper = mount(CNProviderUsageCell, { props: { account } })
    await wrapper.setProps({
      account: { ...account, extra: { ...account.extra, zhipu_5h_used_percent: null, zhipu_weekly_used_percent: null } }
    })
    expect(wrapper.findAllComponents(UsageProgressBar)).toHaveLength(0)
    expect(queryQuota).not.toHaveBeenCalled()
  })

  it('keeps a manual result through unrelated refreshes, then adopts a changed snapshot', async () => {
    queryQuota.mockResolvedValue({ success: true, tiers: [{ window: '5h', used_percent: 44 }] })
    const wrapper = mount(CNProviderUsageCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-usage-probe"]').trigger('click')
    await flushPromises()
    await wrapper.setProps({ account: { ...account, extra: { ...account.extra, unrelated_field: true } } })
    expect(wrapper.findAllComponents(UsageProgressBar).map(bar => bar.props('utilization'))).toEqual([44])
    await wrapper.setProps({ account: { ...account, extra: { ...account.extra, zhipu_5h_used_percent: 66 } } })
    expect(wrapper.findAllComponents(UsageProgressBar).map(bar => bar.props('utilization'))).toEqual([66, 27])
    expect(queryQuota).toHaveBeenCalledTimes(1)
  })

  it('adopts a new snapshot version even if its quota values equal the old snapshot', async () => {
    queryQuota.mockResolvedValue({ success: true, tiers: [{ window: '5h', used_percent: 44 }] })
    const wrapper = mount(CNProviderUsageCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-usage-probe"]').trigger('click')
    await flushPromises()
    await wrapper.setProps({
      account: { ...account, extra: { ...account.extra, zhipu_usage_updated_at: new Date(Date.now() + 1000).toISOString() } }
    })
    expect(wrapper.findAllComponents(UsageProgressBar).map(bar => bar.props('utilization'))).toEqual([0, 27])
  })

  it('keeps the new account snapshot when the previous account probe finishes late', async () => {
    let resolveProbe!: (value: unknown) => void
    queryQuota.mockReturnValue(new Promise(resolve => { resolveProbe = resolve }))
    const wrapper = mount(CNProviderUsageCell, { props: { account } })
    await wrapper.get('[data-test="cn-provider-usage-probe"]').trigger('click')
    await wrapper.setProps({ account: { ...account, id: 8, extra: { ...account.extra, zhipu_5h_used_percent: 91 } } })
    expect(wrapper.findAllComponents(UsageProgressBar)[0].props('utilization')).toBe(91)
    resolveProbe({ success: true, tiers: [{ window: '5h', used_percent: 44 }] })
    await flushPromises()
    expect(wrapper.findAllComponents(UsageProgressBar)[0].props('utilization')).toBe(91)
    expect(wrapper.get('[data-test="cn-provider-usage-probe"]').attributes('disabled')).toBeUndefined()
  })
})
