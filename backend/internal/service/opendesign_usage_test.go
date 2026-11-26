package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// 测试凭据解析与认证头构建（cookie 形式 / Bearer 形式 / workspace 选择）。

const testSessionToken = "LJxPZvG7CYmZH3iIHrzbNSMuDQAsvnCV.kT%2FUv1PNyULQiwo9HFMpiPuDNxz5KRV%2Bp%2Fx4QxiWDrs%3D"

func TestParseOpenDesignConsoleSession(t *testing.T) {
	fullCookie := `od_attr=%7B%22s%22%3A%22%22%7D; _ga=GA1.1.1; __Secure-better-auth.session_token=` + testSessionToken + `; ph_phc=abc`
	require.Equal(t, testSessionToken, parseOpenDesignConsoleSession(fullCookie))
	require.Equal(t, testSessionToken, parseOpenDesignConsoleSession("__Secure-better-auth.session_token="+testSessionToken))
	require.Equal(t, testSessionToken, parseOpenDesignConsoleSession(testSessionToken))
	require.Equal(t, testSessionToken, parseOpenDesignConsoleSession("  "+testSessionToken+"  "))
	require.Empty(t, parseOpenDesignConsoleSession(""))
	require.Empty(t, parseOpenDesignConsoleSession("   "))
}

func TestOpenDesignApplyAuth(t *testing.T) {
	cookieClient := &opendesignUsageClient{session: testSessionToken}
	req := httptest.NewRequest(http.MethodGet, "https://amr-api.open-design.ai/api/v1/workspaces", nil)
	cookieClient.applyAuth(req)
	require.Equal(t, opendesignAuthCookieName+"="+testSessionToken, req.Header.Get("Cookie"))
	require.Empty(t, req.Header.Get("Authorization"))

	bearerClient := &opendesignUsageClient{session: "vela_rt_abc123"}
	req2 := httptest.NewRequest(http.MethodGet, "https://amr-api.open-design.ai/api/v1/workspaces", nil)
	bearerClient.applyAuth(req2)
	require.Equal(t, "Bearer vela_rt_abc123", req2.Header.Get("Authorization"))
	require.Empty(t, req2.Header.Get("Cookie"))
}

func TestOpenDesignSessionHasCookieForm(t *testing.T) {
	require.True(t, (&opendesignUsageClient{session: testSessionToken}).sessionHasCookieForm())
	require.False(t, (&opendesignUsageClient{session: "vela_rt_abc"}).sessionHasCookieForm())
	require.False(t, (&opendesignUsageClient{session: "plainkey123"}).sessionHasCookieForm())
}

func TestOpenDesignPickWorkspace(t *testing.T) {
	type wsItem = struct {
		WorkspaceID   string `json:"workspaceId"`
		WorkspaceName string `json:"workspaceName"`
		IsDefault     bool   `json:"isDefault"`
	}
	// isDefault 优先
	ws, err := openDesignPickWorkspace([]wsItem{
		{WorkspaceID: "ws_b", IsDefault: false},
		{WorkspaceID: "ws_a", IsDefault: true},
	})
	require.NoError(t, err)
	require.Equal(t, "ws_a", ws)
	// 无 default 时取首个有效
	ws, err = openDesignPickWorkspace([]wsItem{{WorkspaceID: "ws_b"}, {WorkspaceID: "ws_c"}})
	require.NoError(t, err)
	require.Equal(t, "ws_b", ws)
	// 空列表报错
	_, err = openDesignPickWorkspace(nil)
	require.Error(t, err)
}

func TestOpenDesignUsageSupportedWorkspaceOptional(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenDesign, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":         "vela_rt_abc",
			"base_url":        DefaultOpenDesignBaseURL,
			"console_session": testSessionToken,
		},
	}
	require.True(t, account.opendesignUsageSupported())

	// 无 session 不支持
	noSession := &Account{
		Platform: PlatformOpenDesign, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "vela_rt_abc", "base_url": DefaultOpenDesignBaseURL},
	}
	require.False(t, noSession.opendesignUsageSupported())

	// 整段 Cookie 也能通过校验（parse 提取）
	cookieAccount := &Account{
		Platform: PlatformOpenDesign, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":         "vela_rt_abc",
			"base_url":        DefaultOpenDesignBaseURL,
			"console_session": "__Secure-better-auth.session_token=" + testSessionToken + "; _ga=x",
		},
	}
	require.True(t, cookieAccount.opendesignUsageSupported())
}

// testDirectHTTPUpstream 用给定 http client 直连（跳过代理与并发池），供 httptest 使用。
type testDirectHTTPUpstream struct {
	client func() *http.Client
}

func (u testDirectHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client().Do(req)
}

func (u testDirectHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// cookie 形式端到端：httptest server 验证 get() 发 Cookie 头并成功解析。
// cnValidateProbeURL 在 allowlist 开启时只放行 https 白名单主机，测试用
// 关闭 allowlist 的配置（nil cfg 退化为格式校验，但 http 仍被拒）→ 用
// https httptest.NewTLSServer 并跳过证书校验不可行，这里通过把
// opendesignConsoleBaseOverride 指向 httptest 的 http URL 是不行的；
// 因此本测试只覆盖 applyAuth + get 的行为在 https 场景下与生产一致，
// URL 校验已由 cn_provider_probe_url_test.go 覆盖。
func TestOpenDesignGetUsesCookieAuth(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, opendesignAuthCookieName+"="+testSessionToken, r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"workspaceId":"ws_a","isDefault":true}]}`))
	}))
	defer server.Close()

	origBase := opendesignConsoleBaseValue()
	defer setOpenDesignConsoleBase(origBase)
	setOpenDesignConsoleBase(server.URL)

	// allowlist 开启并放行 httptest 主机（127.0.0.1）
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           true,
				UpstreamHosts:     []string{"127.0.0.1"},
				AllowPrivateHosts: true,
			},
		},
	}
	client := &opendesignUsageClient{session: testSessionToken, cfg: cfg, account: &Account{Platform: PlatformOpenDesign}, httpUpstream: testDirectHTTPUpstream{server.Client}}
	var list opendesignWorkspaceListPayload
	require.NoError(t, client.get(context.Background(), "/api/v1/workspaces", &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, "ws_a", list.Items[0].WorkspaceID)
}
