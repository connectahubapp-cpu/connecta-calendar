package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// API
//   POST   /api/login            {identifier, password} → {memberId, token, name}
//   GET    /api/calendar         画面用データ                      (要ログイン)
//   POST   /api/entries          予定を追加                         (要ログイン)
//   DELETE /api/entries/{id}     ここで追加した予定を取り消す       (要ログイン)
//   POST   /api/sync             GAS からシートの中身 → 書き戻し待ちを返す (合言葉)
//   GET    /healthz
// ログインは Authorization: Bearer <token> と X-Member-Id: <MemberID>
// ============================================================================

type App struct {
	cfg  Config
	st   *Store
	auth *Auth
	gas  *GAS

	pmu     sync.Mutex
	pcache  *Payload
	pver    int64
	pday    string
	kick    chan struct{}
	kickOne sync.Once
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("GET /api/calendar", a.withUser(a.handleCalendar))
	mux.HandleFunc("POST /api/entries", a.withUser(a.handleAddEntry))
	mux.HandleFunc("DELETE /api/entries/{id}", a.withUser(a.handleDeleteEntry))
	mux.HandleFunc("POST /api/sync", a.handleSync)
	return a.cors(mux)
}

// ---- CORS (画面は GitHub Pages、API はここ) ----
func (a *App) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && a.originOK(o) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Member-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) originOK(o string) bool {
	for _, x := range a.cfg.AllowedOrigin {
		if x == "*" || strings.EqualFold(x, strings.TrimRight(o, "/")) {
			return true
		}
	}
	return false
}

type userHandler func(w http.ResponseWriter, r *http.Request, u *User)

func (a *App) withUser(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mid := r.Header.Get("X-Member-Id")
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		u, err := a.auth.Check(ctx, mid, tok)
		if err != nil {
			writeErr(w, 502, "ログインの確認ができませんでした。少しして開き直してください")
			return
		}
		if u == nil {
			writeJSON(w, 401, map[string]any{"ok": false, "error": "ログインしてください", "unauthorized": true})
			return
		}
		if !a.auth.Allowed(u) {
			writeErr(w, 403, errForbidden.Error())
			return
		}
		h(w, r, u)
	}
}

// ---- ログイン ----
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || strings.TrimSpace(in.Identifier) == "" || in.Password == "" {
		writeErr(w, 400, "ID とパスワードを入れてください")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	u, tok, err := a.gas.Login(ctx, strings.TrimSpace(in.Identifier), in.Password)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if !a.auth.Allowed(u) {
		writeErr(w, 403, errForbidden.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "memberId": u.MemberID, "token": tok, "name": u.Name, "role": u.Role})
}

// ---- 画面用データ (データの版と日付が同じあいだは作り直さない) ----
func (a *App) handleCalendar(w http.ResponseWriter, r *http.Request, u *User) {
	p, err := a.payload()
	if err != nil {
		log.Printf("payload: %v", err)
		writeErr(w, 500, "データを読めませんでした")
		return
	}
	now := time.Now().In(jst)
	out := *p
	out.NowMin = now.Hour()*60 + now.Minute()
	out.Served = now.Format("1/2 15:04")
	writeJSON(w, 200, map[string]any{"ok": true, "data": out, "me": u})
}

func (a *App) payload() (*Payload, error) {
	day := todayJST(time.Now())
	ver := a.st.Version()
	a.pmu.Lock()
	defer a.pmu.Unlock()
	if a.pcache != nil && a.pver == ver && a.pday == day {
		return a.pcache, nil
	}
	d, err := a.st.Load()
	if err != nil {
		return nil, err
	}
	a.pcache, a.pver, a.pday = BuildPayload(d, time.Now()), ver, day
	return a.pcache, nil
}

// ---- 予定の追加 ----
type addReq struct {
	Mid       string `json:"mid"`
	Kind      string `json:"kind"`
	Start     string `json:"start"` // YYYY-MM-DD
	End       string `json:"end"`   // 空なら開始日
	AllDay    bool   `json:"allDay"`
	StartTime string `json:"startTime"` // HH:mm (終日でないとき)
	EndTime   string `json:"endTime"`
	Note      string `json:"note"`
}

func parseHM(s string) (int, bool) {
	var h, m int
	if len(s) < 4 {
		return 0, false
	}
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 || h*60+m > 1440 {
		return 0, false
	}
	return h*60 + m, true
}

func (a *App) handleAddEntry(w http.ResponseWriter, r *http.Request, u *User) {
	var in addReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
		writeErr(w, 400, "入力が読めません")
		return
	}
	if !a.st.MemberExists(in.Mid) {
		writeErr(w, 400, "名前を選んでください")
		return
	}
	if in.Kind != "school" && in.Kind != "private" {
		writeErr(w, 400, "区分を選んでください")
		return
	}
	if !validYmd(in.Start) {
		writeErr(w, 400, "開始日を入れてください")
		return
	}
	if in.End == "" {
		in.End = in.Start
	}
	if !validYmd(in.End) || in.End < in.Start {
		writeErr(w, 400, "終了日は開始日と同じか、あとの日にしてください")
		return
	}
	if daysBetween(in.Start, in.End) > 120 {
		writeErr(w, 400, "期間は120日までにしてください")
		return
	}
	e := Entry{ID: newID(), Mid: in.Mid, Kind: in.Kind, SD: in.Start, ED: in.End, SM: 0, EM: 1440, AllDay: in.AllDay, Note: truncate(strings.TrimSpace(in.Note), 200), By: u.MemberID}
	if !in.AllDay {
		sm, ok1 := parseHM(in.StartTime)
		em, ok2 := parseHM(in.EndTime)
		if !ok1 || !ok2 || (in.Start == in.End && em <= sm) {
			writeErr(w, 400, "時間を正しく入れてください（終わりは始まりよりあと）")
			return
		}
		e.SM, e.EM = sm, em
	}
	if err := a.st.AddEntry(e); err != nil {
		log.Printf("add: %v", err)
		writeErr(w, 500, "保存できませんでした")
		return
	}
	a.kickOutbox()
	writeJSON(w, 200, map[string]any{"ok": true, "id": e.ID})
}

func (a *App) handleDeleteEntry(w http.ResponseWriter, r *http.Request, u *User) {
	ok, err := a.st.DeleteEntry(r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, "取り消せませんでした")
		return
	}
	if !ok {
		writeErr(w, 404, "この予定は取り消せません（ここで追加したものだけ取り消せます）")
		return
	}
	a.kickOutbox()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- GAS からの同期 ----
func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	sec := r.Header.Get("X-Sync-Secret")
	if a.cfg.SyncSecret == "" || subtle.ConstantTimeCompare([]byte(sec), []byte(a.cfg.SyncSecret)) != 1 {
		writeErr(w, 403, "forbidden")
		return
	}
	var in struct {
		Applied  []int64   `json:"applied"`
		Snapshot *Snapshot `json:"snapshot"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := a.st.MarkDone(in.Applied); err != nil {
		log.Printf("markDone: %v", err)
	}
	if in.Snapshot != nil {
		if err := a.st.ApplySnapshot(in.Snapshot); err != nil {
			log.Printf("snapshot: %v", err)
			writeErr(w, 500, "snapshot: "+err.Error())
			return
		}
		log.Printf("sync: members=%d entries=%d busy=%d requests=%d", len(in.Snapshot.Members), len(in.Snapshot.Entries), len(in.Snapshot.Busy), len(in.Snapshot.Requests))
	}
	ops, err := a.st.PendingOps(200)
	if err != nil {
		writeErr(w, 500, "outbox")
		return
	}
	if ops == nil {
		ops = []OutOp{}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ops": ops})
}

// ---- 書き戻し: 追加・取り消しの直後に GAS へ頼む (失敗しても GAS の5分ごとの同期で届く) ----
func (a *App) kickOutbox() {
	a.kickOne.Do(func() { a.kick = make(chan struct{}, 1) })
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *App) outboxLoop() {
	a.kickOne.Do(func() { a.kick = make(chan struct{}, 1) })
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-a.kick:
		case <-t.C:
		}
		if a.cfg.GASURL == "" || a.cfg.SyncSecret == "" {
			continue
		}
		ops, err := a.st.PendingOps(50)
		if err != nil || len(ops) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		ids, err := a.gas.Apply(ctx, ops)
		cancel()
		if err != nil {
			log.Printf("write-back: %v (次の同期で再送)", err)
			continue
		}
		_ = a.st.MarkDone(ids)
	}
}

// ---- 小道具 ----
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}
func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "GO-" + time.Now().In(jst).Format("060102") + "-" + strings.ToUpper(hex.EncodeToString(b))
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
