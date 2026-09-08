package identitypg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/identity"
)

// Endpoints 是 provider 的端點。抽成參數是為了測試能用 httptest 假冒 Discord——
// 單元測試打真的 Discord 既不可重現也會被 rate limit,更不該把真憑證放進測試。
type Endpoints struct {
	Authorize string // OAuth2 授權頁(瀏覽器導向)
	Token     string // code → token 交換
	API       string // REST base,取 /users/@me
	CDN       string // 頭像
}

// DiscordEndpoints 是正式環境的預設值。
func DiscordEndpoints() Endpoints {
	//nolint:gosec // G101 誤報:Token 欄位放的是端點 URL,不是憑證
	return Endpoints{
		Authorize: "https://discord.com/oauth2/authorize",
		Token:     "https://discord.com/api/oauth2/token",
		API:       "https://discord.com/api/v10",
		CDN:       "https://cdn.discordapp.com",
	}
}

// providerToken 是 provider 發給我們的憑證(加密後存 identities.*_token_enc)。
type providerToken struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	ExpiresAt    *time.Time
}

// providerUser 是 provider 側的使用者資料。
type providerUser struct {
	ID        string // provider_user_id,UNIQUE(provider, provider_user_id) 的一半
	Username  string
	Email     string
	AvatarURL string
}

// oauthProvider 是本服務對外部身分來源的最小依賴。
// Twitch / YouTube 之後就是多一個實作,Service 一行不改。
type oauthProvider interface {
	AuthorizeURL(state string) string
	Exchange(ctx context.Context, code string) (*providerToken, error)
	Me(ctx context.Context, accessToken string) (*providerUser, error)
}

// discordProvider 是 Discord 的實作。
type discordProvider struct {
	clientID     string
	clientSecret string
	redirectURI  string
	scopes       []string
	ep           Endpoints
	hc           *http.Client
}

var _ oauthProvider = (*discordProvider)(nil)

// maxProviderBody 限制讀取 provider 回應的大小:對方(或中間人)回一個無限大的 body
// 不該把我們的記憶體吃光。使用者資料與 token 回應都遠小於這個值。
const maxProviderBody = 1 << 20

// AuthorizeURL 組授權頁 URL。
func (d *discordProvider) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", d.clientID)
	q.Set("redirect_uri", d.redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(d.scopes, " "))
	q.Set("state", state)
	sep := "?"
	if strings.Contains(d.ep.Authorize, "?") {
		sep = "&"
	}
	return d.ep.Authorize + sep + q.Encode()
}

// Exchange 用 authorization code 換 token。
//
// 錯誤訊息只帶 HTTP 狀態碼,**不帶回應內容也不帶 code**:
// 失敗回應可能回聲我們送出的參數,而那裡面有 client_secret 與 code。
func (d *discordProvider) Exchange(ctx context.Context, code string) (*providerToken, error) {
	form := url.Values{}
	form.Set("client_id", d.clientID)
	form.Set("client_secret", d.clientSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", d.redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ep.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: 建立 token 請求", identity.ErrProviderExchange)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	body, err := d.do(req, "token 交換")
	if err != nil {
		return nil, err
	}

	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: token 回應不是合法 JSON", identity.ErrProviderExchange)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("%w: token 回應缺 access_token", identity.ErrProviderExchange)
	}
	tok := &providerToken{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		Scope:        raw.Scope,
	}
	if raw.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
		tok.ExpiresAt = &exp
	}
	return tok, nil
}

// Me 取 provider 側使用者資料。
func (d *discordProvider) Me(ctx context.Context, accessToken string) (*providerUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.ep.API+"/users/@me", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 建立 users/@me 請求", identity.ErrProviderExchange)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	body, err := d.do(req, "取使用者資料")
	if err != nil {
		return nil, err
	}

	var raw struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
		Avatar     string `json:"avatar"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: 使用者資料不是合法 JSON", identity.ErrProviderExchange)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("%w: 使用者資料缺 id", identity.ErrProviderExchange)
	}
	u := &providerUser{ID: raw.ID, Username: raw.Username, Email: raw.Email}
	if raw.GlobalName != "" {
		u.Username = raw.GlobalName // Discord 新版顯示名優先
	}
	if raw.Avatar != "" {
		u.AvatarURL = fmt.Sprintf("%s/avatars/%s/%s.png", d.ep.CDN, raw.ID, raw.Avatar)
	}
	return u, nil
}

// do 送出請求並讀回 body。所有錯誤都包成 ErrProviderExchange,訊息不含任何憑證。
func (d *discordProvider) do(req *http.Request, what string) ([]byte, error) {
	resp, err := d.hc.Do(req)
	if err != nil {
		// err 來自 net/http,可能含 URL(query 沒有敏感值,form body 不會進 err)。
		return nil, fmt.Errorf("%w: %s 連線失敗", identity.ErrProviderExchange, what)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %s 讀取回應失敗", identity.ErrProviderExchange, what)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: %s 回 HTTP %d", identity.ErrProviderExchange, what, resp.StatusCode)
	}
	return body, nil
}
