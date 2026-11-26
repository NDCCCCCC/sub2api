package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// OpenDesign 套餐窗口与钱包来自控制台两个只读 GET（dsh-opendesign-go 插件通道二同源，
// 2026-11-25 实测复核）。凭据是控制台 session（Bearer），与推理网关的 API Key 相互独立：
//
//	GET {console}/api/v1/workspaces/{workspaceId}/billing/coding-plan-usage
//	  -> {tier, eligible, windows: [{durationSeconds, usedCredits, limitCredits,
//	      remainingCredits, resetsAt, windowStart, resetMode}]}
//	GET {console}/api/v1/billing/summary?timeZone=<tz>   （可选，失败不阻塞）
//	  -> {balanceUsd | totalAvailableCreditsUsd, subscriptionCurrentPeriodEnd}
//
// console 基址 https://amr-api.open-design.ai；session 过期返回 401 {"error":"unauthenticated"}。
// workspaceId 无法从接口列表发现（vela CLI 私有），存于账号凭据 workspace_id。
// 窗口 credits 为十进制字符串；creditsPerUsd 官方实测 10000（见 dsh-opendesign-go
// FALLBACK_CREDITS_PER_USD），美元折算仅供展示。durationSeconds 18000 = 5h，604800 = 7d。

const (
	opendesignConsoleBase = "https://amr-api.open-design.ai"
	// opendesignExtraSuffixBalance 记录钱包余额（USD），供前端展示与余额低提醒。
	opendesignExtraSuffixBalance = "balance_usd"
)

// opendesignConsoleBaseOverride 测试用：替换控制台基址（httptest server）。
var opendesignConsoleBaseOverride *string

func opendesignConsoleBaseValue() string {
	if opendesignConsoleBaseOverride != nil {
		return *opendesignConsoleBaseOverride
	}
	return opendesignConsoleBase
}

func setOpenDesignConsoleBase(base string) {
	copy := base
	opendesignConsoleBaseOverride = &copy
}

var errOpenDesignInvalidResponse = errors.New("invalid opendesign usage response")

// opendesignAuthCookieName 是 OpenDesign 控制台（better-auth）的会话 cookie 名。
// amr-api 接受 Cookie 认证；Bearer 形式仅对 vela/控制台 API Key 有效，作为兜底。
const opendesignAuthCookieName = "__Secure-better-auth.session_token"

// parseOpenDesignConsoleSession 从用户粘贴的内容中提取 better-auth session token 值。
// 接受三种输入（2026-11-26 实测 cookie 形式认证有效）：
//  1. 整段浏览器 Cookie 头（含多对；取 __Secure-better-auth.session_token）
//  2. 单个 "name=value" 对
//  3. 裸 token 值（原样返回）
func parseOpenDesignConsoleSession(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, ";") || strings.Contains(raw, "\n") {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' || r == ' ' }) {
			part = strings.TrimSpace(part)
			if name, value, ok := strings.Cut(part, "="); ok && strings.Contains(name, "session_token") {
				return strings.TrimSpace(value)
			}
		}
	}
	if name, value, ok := strings.Cut(raw, "="); ok && strings.Contains(name, "session_token") && !strings.Contains(name, " ") {
		return strings.TrimSpace(value)
	}
	return raw
}

// opendesignUsageSupported 报告账号能否查询 OpenDesign 用量：API Key 账号、推理端点
// 指向官方主机、且配置了控制台凭据（credentials.console_session；workspace_id 可留空
// 由 /workspaces 接口自动发现）。
func (a *Account) opendesignUsageSupported() bool {
	return a.IsOpenDesign() && a.Type == AccountTypeAPIKey &&
		isOfficialOpenDesignHost(a.GetOpenAIBaseURL()) &&
		parseOpenDesignConsoleSession(a.GetCredential("console_session")) != ""
}

// opendesignUsageClient 查询控制台窗口与钱包。
type opendesignUsageClient struct {
	cfg          *config.Config
	httpUpstream HTTPUpstream
	account      *Account
	session      string // better-auth session token（cookie 形式认证）或 vela/控制台 API Key（Bearer）
	workspace    string // 空值时经 /workspaces 自动发现
	proxyURL     string
}

func newOpenDesignUsageClient(cfg *config.Config, httpUpstream HTTPUpstream, account *Account, proxyURL string) *opendesignUsageClient {
	return &opendesignUsageClient{
		cfg:          cfg,
		httpUpstream: httpUpstream,
		account:      account,
		session:      parseOpenDesignConsoleSession(account.GetCredential("console_session")),
		workspace:    strings.TrimSpace(account.GetCredential("workspace_id")),
		proxyURL:     proxyURL,
	}
}

// opendesignHTTPError 是控制台接口的非 2xx 响应。
type opendesignHTTPError struct {
	status int
	body   string
}

func (e *opendesignHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

func (e *opendesignHTTPError) unauthorized() bool {
	return e.status == http.StatusUnauthorized || e.status == http.StatusForbidden
}

func isOpenDesignAuthError(err error) bool {
	var httpErr *opendesignHTTPError
	return errors.As(err, &httpErr) && httpErr.unauthorized()
}

// opendesignWindowRaw 是上游窗口对象；credits 为十进制字符串（保留原样校验）。
type opendesignWindowRaw struct {
	DurationSeconds  float64 `json:"durationSeconds"`
	UsedCredits      string  `json:"usedCredits"`
	LimitCredits     string  `json:"limitCredits"`
	RemainingCredits string  `json:"remainingCredits"`
	ResetsAt         string  `json:"resetsAt"`
}

// opendesignUsagePayload 是 coding-plan-usage 响应。
type opendesignUsagePayload struct {
	Tier     string                `json:"tier"`
	Eligible bool                  `json:"eligible"`
	Windows  []opendesignWindowRaw `json:"windows"`
}

// opendesignSummaryPayload 是 billing/summary 响应（钱包）。
type opendesignSummaryPayload struct {
	BalanceUsd                   string `json:"balanceUsd"`
	TotalAvailableCreditsUsd     string `json:"totalAvailableCreditsUsd"`
	SubscriptionCurrentPeriodEnd string `json:"subscriptionCurrentPeriodEnd"`
}

// get 请求一个控制台接口并解码 JSON。
func (c *opendesignUsageClient) get(ctx context.Context, path string, out any) error {
	target := opendesignConsoleBaseValue() + path
	validatedURL, err := cnValidateProbeURL(c.cfg, target)
	if err != nil {
		return infraerrors.New(http.StatusForbidden, "CN_QUOTA_URL_REJECTED", err.Error())
	}
	callCtx, cancel := context.WithTimeout(ctx, cnQuotaUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, validatedURL, nil)
	if err != nil {
		return infraerrors.Newf(http.StatusInternalServerError, "CN_QUOTA_REQUEST_BUILD_FAILED", "build request: %v", err)
	}
	c.applyAuth(req)
	c.account.ApplyHeaderOverrides(req.Context(), req.Header)

	resp, err := c.httpUpstream.Do(req, c.proxyURL, c.account.ID, maxInt(c.account.Concurrency, 1))
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cnQuotaMaxBodyBytes))
	// 401 且是 cookie 形式时用 Bearer 兜底重试一次（兼容控制台 API Key 凭据）。
	if resp.StatusCode == http.StatusUnauthorized && c.sessionHasCookieForm() {
		_ = resp.Body.Close()
		retry, err := http.NewRequestWithContext(callCtx, http.MethodGet, validatedURL, nil)
		if err == nil {
			retry.Header.Set("Authorization", "Bearer "+c.session)
			retry.Header.Set("Accept", "application/json")
			c.account.ApplyHeaderOverrides(req.Context(), retry.Header)
			resp, err = c.httpUpstream.Do(retry, c.proxyURL, c.account.ID, maxInt(c.account.Concurrency, 1))
			if err != nil {
				return fmt.Errorf("upstream request failed: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ = io.ReadAll(io.LimitReader(resp.Body, cnQuotaMaxBodyBytes))
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &opendesignHTTPError{status: resp.StatusCode, body: truncate(strings.TrimSpace(string(body)), 240)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s: %v", errOpenDesignInvalidResponse, path, err)
	}
	return nil
}

// sessionHasCookieForm 报告凭据是否为 better-auth session token（Cookie 形式认证）。
// cookie 值含一个点分隔的签名段（value.signature），且不含 vela/控制台 API Key 前缀特征。
func (c *opendesignUsageClient) sessionHasCookieForm() bool {
	return strings.Contains(c.session, ".") && !strings.HasPrefix(c.session, "vela_")
}

// applyAuth 设置控制台请求的鉴权头：better-auth session 走 Cookie（2026-11-26 实测
// Bearer 对 session token 返回 401）；其他值（vela/控制台 API Key）走 Bearer。
func (c *opendesignUsageClient) applyAuth(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	if c.sessionHasCookieForm() {
		req.Header.Set("Cookie", opendesignAuthCookieName+"="+c.session)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.session)
}

// opendesignWorkspaceListPayload 是 /workspaces 响应（workspace 自动发现用）。
type opendesignWorkspaceListPayload struct {
	Items []struct {
		WorkspaceID   string `json:"workspaceId"`
		WorkspaceName string `json:"workspaceName"`
		IsDefault     bool   `json:"isDefault"`
	} `json:"items"`
}

// resolveWorkspace 返回探测用 workspace ID；凭据未填时经 /workspaces 自动发现
// （默认工作区优先，个人账号通常只有一个）。
func (c *opendesignUsageClient) resolveWorkspace(ctx context.Context) (string, error) {
	if c.workspace != "" {
		return c.workspace, nil
	}
	var list opendesignWorkspaceListPayload
	if err := c.get(ctx, "/api/v1/workspaces", &list); err != nil {
		return "", err
	}
	return openDesignPickWorkspace(list.Items)
}

// openDesignPickWorkspace 从工作区列表选探测目标：isDefault 优先，否则首个有效 ID。
func openDesignPickWorkspace(items []struct {
	WorkspaceID   string `json:"workspaceId"`
	WorkspaceName string `json:"workspaceName"`
	IsDefault     bool   `json:"isDefault"`
}) (string, error) {
	best := ""
	for _, item := range items {
		if strings.TrimSpace(item.WorkspaceID) == "" {
			continue
		}
		if item.IsDefault {
			return item.WorkspaceID, nil
		}
		if best == "" {
			best = item.WorkspaceID
		}
	}
	if best == "" {
		return "", fmt.Errorf("%w: workspace list has no usable id", errOpenDesignInvalidResponse)
	}
	return best, nil
}

// fetch 取窗口与钱包；workspace 未配置时自动发现。summary 失败不阻塞（钱包是展示性数据）。
func (c *opendesignUsageClient) fetch(ctx context.Context) (*opendesignUsagePayload, *opendesignSummaryPayload, error) {
	workspace, err := c.resolveWorkspace(ctx)
	if err != nil {
		return nil, nil, err
	}
	var usage opendesignUsagePayload
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/billing/coding-plan-usage"
	if err := c.get(ctx, path, &usage); err != nil {
		return nil, nil, err
	}
	var summaryRaw opendesignSummaryPayload
	if err := c.get(ctx, "/api/v1/billing/summary?timeZone=Asia/Shanghai", &summaryRaw); err == nil {
		return &usage, &summaryRaw, nil
	}
	return &usage, nil, nil
}

// opendesignCreditsToNumber 解析十进制 credits 字符串；畸形值返回失败标记。
func opendesignCreditsToNumber(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// opendesignWindowLabel 按 durationSeconds 反推窗口名（18,000s → 5h；604,800s → weekly）。
// 不硬编码套餐名：窗口长度变化时描述自动跟随（与插件 compactWindowLabel 同规则）。
func opendesignWindowLabel(durationSeconds float64) string {
	switch {
	case durationSeconds <= 0:
		return ""
	case durationSeconds <= 3600*6:
		return "5h"
	case durationSeconds <= 3600*24*8:
		return "weekly"
	case durationSeconds <= 3600*24*31:
		return "monthly"
	default:
		return ""
	}
}

// tiers 把上游窗口转成 CNQuotaTier；必填字段缺失的窗口丢弃，不补 0。
func (u *opendesignUsagePayload) tiers() []CNQuotaTier {
	tiers := make([]CNQuotaTier, 0, len(u.Windows))
	for _, w := range u.Windows {
		window := opendesignWindowLabel(w.DurationSeconds)
		if window == "" {
			continue
		}
		used, okUsed := opendesignCreditsToNumber(w.UsedCredits)
		limit, okLimit := opendesignCreditsToNumber(w.LimitCredits)
		if !okUsed || !okLimit || limit <= 0 {
			continue
		}
		resetAt := strings.TrimSpace(w.ResetsAt)
		if resetAt == "" {
			continue
		}
		percent := used / limit * 100
		if percent < 0 {
			percent = 0
		}
		tiers = append(tiers, CNQuotaTier{
			Window:      window,
			UsedPercent: math.Round(percent*100) / 100,
			ResetAt:     cnNormalizeResetTime(resetAt),
		})
	}
	return tiers
}

// opendesignBalanceResult 构造钱包余额快照；summary 缺席时返回成功但余额不可用
// （窗口快照仍然有效）。
func opendesignBalanceResult(summary *opendesignSummaryPayload, now time.Time) *CNProviderBalanceResult {
	result := &CNProviderBalanceResult{
		Provider:   PlatformOpenDesign,
		Success:    true,
		Available:  summary != nil,
		StatusCode: http.StatusOK,
		FetchedAt:  now.Unix(),
	}
	if summary == nil {
		return result
	}
	balance := firstNonEmpty(summary.BalanceUsd, summary.TotalAvailableCreditsUsd)
	if balance == "" {
		return result
	}
	value, ok := opendesignCreditsToNumber(balance)
	if !ok {
		return result
	}
	result.Balance = value
	result.Currency = "USD"
	result.Balances = []CNProviderBalanceEntry{{Currency: "USD", Balance: value}}
	return result
}

// opendesignUsageExtraUpdates 构造窗口 + 余额快照；缺席的窗口写 nil（套餐变化后
// 不沿用旧快照），summary 失败保留旧余额。usage 为 nil 时只写余额。
func opendesignUsageExtraUpdates(usage *opendesignUsagePayload, summary *opendesignSummaryPayload, tiers []CNQuotaTier, balance *CNProviderBalanceResult, now time.Time) map[string]any {
	updates := cnBalanceExtraUpdates(PlatformOpenDesign, balance, now)
	if summary != nil {
		if balanceValue, ok := opendesignCreditsToNumber(firstNonEmpty(summary.BalanceUsd, summary.TotalAvailableCreditsUsd)); ok {
			updates[cnExtraKey(PlatformOpenDesign, opendesignExtraSuffixBalance)] = balanceValue
		}
		if periodEnd := strings.TrimSpace(summary.SubscriptionCurrentPeriodEnd); periodEnd != "" {
			updates[cnExtraKey(PlatformOpenDesign, "subscription_period_end")] = cnNormalizeResetTime(periodEnd)
		}
	}
	if usage == nil {
		return updates
	}
	for key, value := range cnQuotaExtraUpdates(PlatformOpenDesign, tiers, now) {
		updates[key] = value
	}
	present := make(map[string]CNQuotaTier, len(tiers))
	for _, tier := range tiers {
		present[tier.Window] = tier
	}
	for _, window := range []struct {
		name, used, reset string
	}{
		{"5h", cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
		{"weekly", cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
	} {
		tier, ok := present[window.name]
		if !ok {
			updates[cnExtraKey(PlatformOpenDesign, window.used)] = nil
		}
		if tier.ResetAt == "" {
			updates[cnExtraKey(PlatformOpenDesign, window.reset)] = nil
		}
	}
	return updates
}

// opendesignFetchFailure 把取数错误转换为探测失败文案；ok=false 表示应作为错误返回。
func opendesignFetchFailure(err error) (status int, message string, ok bool) {
	var httpErr *opendesignHTTPError
	switch {
	case errors.As(err, &httpErr) && httpErr.unauthorized():
		// session 过期/无效：提示重新配置（凭据与推理 Key 相互独立）。
		return httpErr.status, "Console session expired or invalid, please update console_session", true
	case errors.As(err, &httpErr):
		return httpErr.status, fmt.Sprintf("API error (HTTP %d): %s", httpErr.status, httpErr.body), true
	case errors.Is(err, errOpenDesignInvalidResponse):
		return http.StatusOK, "Invalid usage response", true
	default:
		return 0, "", false
	}
}

// queryOpenDesignUsage 查询 OpenDesign 窗口与钱包，落窗口与余额快照。
func (s *CNProviderQuotaService) queryOpenDesignUsage(ctx context.Context, account *Account) (*CNProviderQuotaProbeResult, error) {
	client := newOpenDesignUsageClient(s.cfg, s.httpUpstream, account, s.resolveProxyURL(ctx, account))
	if client.session == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_QUOTA_NO_SESSION",
			"account needs console_session credentials (browser cookie value) for usage probing")
	}
	now := time.Now().UTC()
	result := &CNProviderQuotaProbeResult{
		Provider:  PlatformOpenDesign,
		Source:    "opendesign_usage",
		FetchedAt: now.Unix(),
	}
	usage, summary, err := client.fetch(ctx)
	if err != nil {
		status, message, ok := opendesignFetchFailure(err)
		if !ok {
			if isApplicationError(err) {
				return nil, err
			}
			return nil, infraerrors.Newf(http.StatusBadGateway, "CN_QUOTA_REQUEST_FAILED", "%v", err)
		}
		result.StatusCode = status
		result.Error = message
		return result, nil
	}

	tiers := usage.tiers()
	if !usage.Eligible || len(tiers) == 0 {
		result.StatusCode = http.StatusOK
		result.Error = "console returned no usable quota window"
		return result, nil
	}
	balance := opendesignBalanceResult(summary, now)
	result.StatusCode = http.StatusOK
	result.Tiers = tiers
	result.PlanLevel = strings.TrimSpace(usage.Tier)
	result.Balance = balance
	result.Success = true
	result.CredentialValid = true

	updates := opendesignUsageExtraUpdates(usage, summary, tiers, balance, now)
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("cn_quota_persist_failed", "account_id", account.ID, "provider", PlatformOpenDesign, "error", err)
	} else {
		result.Persisted = true
		balance.Persisted = true
	}
	return result, nil
}

// queryOpenDesignBalance 只查钱包余额（窗口快照不动）。
func (s *CNProviderBalanceService) queryOpenDesignBalance(ctx context.Context, account *Account) (*CNProviderBalanceResult, error) {
	client := newOpenDesignUsageClient(s.cfg, s.httpUpstream, account, s.resolveProxyURL(ctx, account))
	if client.session == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "CN_BALANCE_NO_SESSION",
			"account needs console_session credentials (browser cookie value) for balance probing")
	}
	now := time.Now().UTC()
	_, summary, err := client.fetch(ctx)
	if err != nil {
		status, message, ok := opendesignFetchFailure(err)
		if !ok {
			if isApplicationError(err) {
				return nil, err
			}
			return nil, infraerrors.Newf(http.StatusBadGateway, "CN_BALANCE_REQUEST_FAILED", "%v", err)
		}
		return &CNProviderBalanceResult{
			Provider:   PlatformOpenDesign,
			StatusCode: status,
			FetchedAt:  now.Unix(),
			Available:  true,
			Error:      message,
		}, nil
	}
	result := opendesignBalanceResult(summary, now)
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, opendesignUsageExtraUpdates(nil, summary, nil, result, now)); err != nil {
		slog.Warn("cn_balance_persist_failed", "account_id", account.ID, "provider", PlatformOpenDesign, "error", err)
	} else {
		result.Persisted = true
	}
	return result, nil
}
