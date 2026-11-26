import { describe, expect, it } from 'vitest'

// 调度阈值平台清单：系统设置页与账号编辑页共用，必须与后端
// AllowedSchedulingThresholdPlatforms 一致（含 opendesign，不含 deepseek）。

import {
  SCHEDULING_THRESHOLD_PLATFORMS,
  normalizeAccountSchedulingThresholdsMap,
  sanitizeAccountSchedulingThresholdsMap
} from '@/api/admin/settings'

describe('SCHEDULING_THRESHOLD_PLATFORMS', () => {
  it('包含 opendesign（与后端白名单一致）', () => {
    expect(SCHEDULING_THRESHOLD_PLATFORMS).toContain('opendesign')
  })

  it('不包含 deepseek（余额型，走余额检测）', () => {
    expect(SCHEDULING_THRESHOLD_PLATFORMS).not.toContain('deepseek')
  })

  it('保留既有套餐平台', () => {
    for (const platform of ['openai', 'anthropic', 'grok', 'kimi', 'zhipu', 'minimax', 'opencode_go', 'command_code']) {
      expect(SCHEDULING_THRESHOLD_PLATFORMS).toContain(platform)
    }
  })
})

describe('normalizeAccountSchedulingThresholdsMap', () => {
  it('为 opendesign 生成默认 100（opt-in，不改变既有契约）', () => {
    const map = normalizeAccountSchedulingThresholdsMap({})
    expect(map.opendesign).toBe(100)
    // 既有平台默认值不变
    expect(map.openai).toBe(100)
    expect(map.anthropic).toBe(100)
    expect(map.grok).toBe(100)
  })

  it('保留 opendesign 的 1..100 取值并夹紧越界值', () => {
    expect(normalizeAccountSchedulingThresholdsMap({ opendesign: 50 } as any).opendesign).toBe(50)
    expect(normalizeAccountSchedulingThresholdsMap({ opendesign: 0 } as any).opendesign).toBe(1)
    expect(normalizeAccountSchedulingThresholdsMap({ opendesign: 200 } as any).opendesign).toBe(100)
    expect(normalizeAccountSchedulingThresholdsMap({ opendesign: 49.7 } as any).opendesign).toBe(49)
  })

  it('sanitize 与 normalize 结果一致', () => {
    const input = { opendesign: 55, zhipu: 40 } as any
    expect(sanitizeAccountSchedulingThresholdsMap(input)).toEqual(normalizeAccountSchedulingThresholdsMap(input))
  })
})
