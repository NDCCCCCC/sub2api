package service

import (
	"net/url"
	"strings"
)

// OpenDesign Cloud（https://open-design.ai）是 AI 设计 Agent 工作台，其 Cloud 订阅
// （Go/Plus/Pro/Max 档）通过 amr-link.open-design.ai 网关提供多模型推理：
//   - Chat Completions 与 Responses 均在 {base}/v1 下（base = https://amr-link.open-design.ai）；
//   - Anthropic Messages 原生端点为 https://amr-link.open-design.ai（拼接 /v1/messages）。
//
// 鉴权统一 Bearer Key（控制台 API Key 或 vela runtime key，均为 amr-link 通用 Key）。
// 套餐积分按两个锚定滚动窗口（5 小时 / 7 天）限制，另有钱包余额；无独立余额查询
// REST 接口（vela CLI 私有），窗口用量走控制台 session（见 opendesign_usage.go）。

// DefaultOpenDesignTestModel 是管理端连接测试未指定模型时使用的模型（Chat Completions）。
const DefaultOpenDesignTestModel = "deepseek-v4-flash"

// isOfficialOpenDesignHost 报告 base_url 是否指向 OpenDesign 官方主机。自定义中转
// 的 Key 不得发往官方控制台接口（与 isOfficialCommandCodeHost 同一安全约束）。
func isOfficialOpenDesignHost(targetURL string) bool {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), "amr-link.open-design.ai")
}

// IsOpenDesign 报告账号是否为 OpenDesign 平台账号。
func (a *Account) IsOpenDesign() bool {
	return a != nil && a.Platform == PlatformOpenDesign
}
