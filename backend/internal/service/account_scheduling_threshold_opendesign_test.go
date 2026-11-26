package service

// 账号级 / 平台级自动停调阈值：套餐类平台（Kimi / GLM / MiniMax / OpenCode /
// Command Code / OpenDesign）的 5h 与 7d 窗口独立判定，取命中窗口里最早的重置
// 时间——5h 重置后即可恢复使用 5h 额度，若更宽窗口仍超阈值，下一轮评估会续停。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAllowedSchedulingThresholdPlatformsIncludesOpenDesign(t *testing.T) {
	t.Parallel()
	require.Contains(t, AllowedSchedulingThresholdPlatforms, PlatformOpenDesign)
	// deepseek 是余额型，刻意不在白名单内。
	require.NotContains(t, AllowedSchedulingThresholdPlatforms, PlatformDeepseek)
}

func TestValidateAccountSchedulingThresholdsAcceptsOpenDesign(t *testing.T) {
	t.Parallel()
	normalized, err := validateAndNormalizeAccountSchedulingThresholds(map[string]int{
		PlatformOpenDesign: 50,
		PlatformZhipu:      50,
	})
	require.NoError(t, err)
	require.Equal(t, 50, normalized[PlatformOpenDesign])
	require.Equal(t, 50, normalized[PlatformZhipu])
	// 默认 map 不变：未配置的平台仍是 100（opt-in，不改变既有系统设置契约）。
	require.Equal(t, 100, normalized[PlatformOpenAI])
}

func TestParseAccountSchedulingThresholdsSettingKeepsOpenDesign(t *testing.T) {
	t.Parallel()
	thresholds, err := parseAccountSchedulingThresholdsSetting(`{"opendesign":50,"zhipu":40}`)
	require.NoError(t, err)
	require.Equal(t, 50, thresholds[PlatformOpenDesign])
	require.Equal(t, 40, thresholds[PlatformZhipu])
}

// OpenDesign 5h 用量达阈值 → 停到 5h 重置点。
func TestEvaluateAccountSchedulingThreshold_OpenDesignFiveHour(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	account := &Account{
		Platform: PlatformOpenDesign,
		Extra: map[string]any{
			"opendesign_5h_used_percent":     55.0,
			"opendesign_5h_reset_at":         reset.Format(time.RFC3339),
			"opendesign_weekly_used_percent": 30.0,
			"opendesign_weekly_reset_at":     now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 50}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, PlatformOpenDesign, decision.Platform)
	require.Equal(t, "5h", decision.Window)
	require.InDelta(t, 55.0, decision.UsedPercent, 1e-9)
	require.Equal(t, 50, decision.ThresholdPercent)
	require.NotNil(t, decision.Until)
	require.True(t, reset.Equal(*decision.Until))
}

// 5h 与 7d 同时超阈值 → 取最早重置（5h），而不是最晚（7d）。
func TestEvaluateAccountSchedulingThreshold_OpenDesignBothWindowsPicksEarliest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(2 * time.Hour)
	weeklyReset := now.Add(5 * 24 * time.Hour)
	account := &Account{
		Platform: PlatformOpenDesign,
		Extra: map[string]any{
			"opendesign_5h_used_percent":     60.0,
			"opendesign_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			"opendesign_weekly_used_percent": 70.0,
			"opendesign_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 50}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "5h", decision.Window)
	require.True(t, fiveHourReset.Equal(*decision.Until))
}

// 仅 7d 超阈值 → 停到 7d 重置。
func TestEvaluateAccountSchedulingThreshold_OpenDesignWeeklyOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(5 * 24 * time.Hour)
	account := &Account{
		Platform: PlatformOpenDesign,
		Extra: map[string]any{
			"opendesign_5h_used_percent":     10.0,
			"opendesign_5h_reset_at":         now.Add(2 * time.Hour).Format(time.RFC3339),
			"opendesign_weekly_used_percent": 51.0,
			"opendesign_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 50}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "weekly", decision.Window)
	require.True(t, weeklyReset.Equal(*decision.Until))
}

// 窗口已重置（reset <= now）→ 不停调，账号自然回到调度池。
func TestEvaluateAccountSchedulingThreshold_OpenDesignResetExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	account := &Account{
		Platform: PlatformOpenDesign,
		Extra: map[string]any{
			"opendesign_5h_used_percent": 99.0,
			"opendesign_5h_reset_at":     now.Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}
	require.False(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 50}, now).ShouldPause)
}

// 系统阈值 100（禁用）→ 不停调。
func TestEvaluateAccountSchedulingThreshold_OpenDesignDisabledByHundred(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	account := &Account{
		Platform: PlatformOpenDesign,
		Extra: map[string]any{
			"opendesign_5h_used_percent": 90.0,
			"opendesign_5h_reset_at":     now.Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	require.False(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 100}, now).ShouldPause)
}

// 账号级覆盖优先于系统阈值：账号 100 可禁用系统 50。
func TestEvaluateAccountSchedulingThreshold_OpenDesignAccountOverrideDisables(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	account := &Account{
		Platform: PlatformOpenDesign,
		Credentials: map[string]any{
			"account_scheduling_threshold": 100,
		},
		Extra: map[string]any{
			"opendesign_5h_used_percent": 90.0,
			"opendesign_5h_reset_at":     now.Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 50}, now)
	require.False(t, decision.ShouldPause)
	require.Equal(t, 100, decision.ThresholdPercent)
}

// 账号级覆盖低于系统阈值时按账号值生效。
func TestEvaluateAccountSchedulingThreshold_OpenDesignAccountOverrideLower(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	account := &Account{
		Platform: PlatformOpenDesign,
		Credentials: map[string]any{
			"account_scheduling_threshold": 40,
		},
		Extra: map[string]any{
			"opendesign_5h_used_percent": 45.0,
			"opendesign_5h_reset_at":     reset.Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenDesign: 80}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, 40, decision.ThresholdPercent)
	require.True(t, reset.Equal(*decision.Until))
}

// GLM（zhipu）同样取最早重置：验证套餐平台统一语义。
func TestEvaluateAccountSchedulingThreshold_ZhipuPicksEarliestReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(90 * time.Minute)
	account := &Account{
		Platform: PlatformZhipu,
		Credentials: map[string]any{
			"account_mode":                 "coding",
			"account_scheduling_threshold": 50,
		},
		Extra: map[string]any{
			"zhipu_5h_used_percent":     52.0,
			"zhipu_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			"zhipu_weekly_used_percent": 88.0,
			"zhipu_weekly_reset_at":     now.Add(4 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformZhipu: 100}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "5h", decision.Window)
	require.True(t, fiveHourReset.Equal(*decision.Until))
}
