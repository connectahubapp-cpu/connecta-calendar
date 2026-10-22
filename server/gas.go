package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ============================================================================
// GAS とのやりとり: ログイン (authenticate)・本人確認 (whoami)・書き戻し (calendarSync.apply)
// ============================================================================

type GAS struct {
	url    string
	secret string
	hc     *http.Client
}

func NewGAS(cfg Config) *GAS {
	return &GAS{url: cfg.GASURL, secret: cfg.SyncSecret, hc: &http.Client{Timeout: 45 * time.Second}}
}

// GAS のウェブアプリは 302 で googleusercontent.com へ飛ばす。POST も GET で追いかければ結果が読める (Go の既定の動き)。
func (g *GAS) do(ctx context.Context, method string, q url.Values, body interface{}) (map[string]interface{}, error) {
	if g.url == "" {
		return nil, errors.New("GAS_URL が未設定です")
	}
	u := g.url
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/plain;charset=utf-8")
	}
	res, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("GAS HTTP %d", res.StatusCode)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("GAS の応答が読めません")
	}
	return out, nil
}

// ---- ログイン ----
type User struct {
	MemberID string `json:"memberId"`
	Name     string `json:"name"`
	Rank     int    `json:"rank"`
	Role     string `json:"role"`
}

func userFrom(v interface{}) *User {
	m, _ := v.(map[string]interface{})
	if m == nil {
		return nil
	}
	u := &User{}
	if mem, _ := m["member"].(map[string]interface{}); mem != nil {
		u.MemberID, _ = mem["id"].(string)
		if n, ok := mem["name"].(string); ok {
			u.Name = n
		} else if n, ok := mem["氏名"].(string); ok {
			u.Name = n
		}
	}
	if u.MemberID == "" {
		u.MemberID, _ = m["memberId"].(string)
	}
	if r, ok := m["highestRank"].(float64); ok {
		u.Rank = int(r)
	}
	u.Role, _ = m["highestRole"].(string)
	if u.MemberID == "" {
		return nil
	}
	return u
}

// Login: Console と同じ ID・パスワードで GAS の authenticate を呼ぶ
func (g *GAS) Login(ctx context.Context, identifier, password string) (*User, string, error) {
	out, err := g.do(ctx, http.MethodPost, nil, map[string]interface{}{"action": "authenticate", "payload": map[string]string{"identifier": identifier, "password": password}})
	if err != nil {
		return nil, "", err
	}
	res := out
	if d, ok := out["data"].(map[string]interface{}); ok { // {ok, data:{ok, token, user}} の形にも対応
		res = d
	}
	if ok, _ := res["ok"].(bool); !ok {
		msg, _ := res["error"].(string)
		if msg == "" {
			msg, _ = out["error"].(string)
		}
		if msg == "" {
			msg = "ログインできませんでした"
		}
		return nil, "", errors.New(msg)
	}
	tok, _ := res["token"].(string)
	u := userFrom(res["user"])
	if tok == "" || u == nil {
		return nil, "", errors.New("ログインの応答が読めません")
	}
	return u, tok, nil
}

// WhoAmI: トークンが今も有効か (パスワード変更などで無効になる)
func (g *GAS) WhoAmI(ctx context.Context, memberID, token string) (*User, error) {
	q := url.Values{"action": {"whoami"}, "memberId": {memberID}, "token": {token}}
	out, err := g.do(ctx, http.MethodGet, q, nil)
	if err != nil {
		return nil, err
	}
	u := userFrom(out["user"])
	if u == nil || u.MemberID != memberID {
		return nil, nil
	}
	return u, nil
}

// Apply: 書き戻しを頼む。適用された op の id が返る
func (g *GAS) Apply(ctx context.Context, ops []OutOp) ([]int64, error) {
	out, err := g.do(ctx, http.MethodPost, nil, map[string]interface{}{"action": "calendarSync.apply", "secret": g.secret, "ops": ops})
	if err != nil {
		return nil, err
	}
	if ok, _ := out["ok"].(bool); !ok {
		msg, _ := out["error"].(string)
		return nil, fmt.Errorf("書き戻しに失敗: %s", msg)
	}
	var ids []int64
	if arr, ok := out["applied"].([]interface{}); ok {
		for _, x := range arr {
			if f, ok := x.(float64); ok {
				ids = append(ids, int64(f))
			}
		}
	}
	return ids, nil
}

// ============================================================================
// 本人確認の結果を5分覚える (毎回 GAS に聞かない)
// ============================================================================
type Auth struct {
	gas     *GAS
	minRank int
	mu      sync.Mutex
	cache   map[string]authHit
}
type authHit struct {
	u   *User
	exp time.Time
}

func NewAuth(cfg Config) *Auth {
	return &Auth{gas: NewGAS(cfg), minRank: cfg.MinRank, cache: map[string]authHit{}}
}

var errForbidden = errors.New("マネージャー以上の人だけが見られます")

func (a *Auth) Check(ctx context.Context, memberID, token string) (*User, error) {
	if memberID == "" || token == "" {
		return nil, nil
	}
	h := sha256.Sum256([]byte(memberID + "\x00" + token))
	key := hex.EncodeToString(h[:])
	a.mu.Lock()
	if hit, ok := a.cache[key]; ok && time.Now().Before(hit.exp) {
		a.mu.Unlock()
		return hit.u, nil
	}
	a.mu.Unlock()
	u, err := a.gas.WhoAmI(ctx, memberID, token)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if len(a.cache) > 2000 {
		a.cache = map[string]authHit{}
	}
	a.cache[key] = authHit{u: u, exp: time.Now().Add(5 * time.Minute)}
	a.mu.Unlock()
	return u, nil
}

func (a *Auth) Allowed(u *User) bool { return u != nil && u.Rank >= a.minRank }
