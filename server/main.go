// connecta-calendar: 空き状況カレンダーのバックエンド (Render)。
//
//	画面 (GitHub Pages) ──HTTP──▶ この Go ──SQLite (保存)
//	                                  ▲  │
//	         GAS がシートの中身を送る ─┘  └─▶ Go で追加・取り消した予定を GAS へ書き戻し
//
// 環境変数:
//
//	PORT           待ち受けるポート (既定 8080)
//	DB_PATH        SQLite のファイル (既定 ./calendar.db。消えても GAS から取り直す)
//	GAS_URL        GAS ウェブアプリの /exec の URL (ログイン確認・書き戻し)
//	SYNC_SECRET    GAS と共有する合言葉 (GAS の CALSYNC_SECRET と同じ)
//	ALLOWED_ORIGIN 画面の置き場所 (例 https://connectahubapp-cpu.github.io)。カンマ区切りで複数可
//	MIN_RANK       見られる役職の下限 (既定 3 = マネージャー以上)
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port          string
	DBPath        string
	GASURL        string
	SyncSecret    string
	AllowedOrigin []string
	MinRank       int
}

func loadConfig() Config {
	c := Config{
		Port:       env("PORT", "8080"),
		DBPath:     env("DB_PATH", "calendar.db"),
		GASURL:     strings.TrimSpace(os.Getenv("GAS_URL")),
		SyncSecret: os.Getenv("SYNC_SECRET"),
		MinRank:    3,
	}
	for _, o := range strings.Split(env("ALLOWED_ORIGIN", "*"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.AllowedOrigin = append(c.AllowedOrigin, o)
		}
	}
	if n, err := strconv.Atoi(os.Getenv("MIN_RANK")); err == nil && n > 0 {
		c.MinRank = n
	}
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := loadConfig()
	if cfg.SyncSecret == "" {
		log.Println("注意: SYNC_SECRET が空です。GAS からの同期を受け付けません")
	}
	st, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("DB を開けません: %v", err)
	}
	defer st.Close()

	app := &App{cfg: cfg, st: st, auth: NewAuth(cfg), gas: NewGAS(cfg)}
	go app.outboxLoop()
	go app.warmUp()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	log.Printf("listening on :%s (db=%s)", cfg.Port, cfg.DBPath)
	log.Fatal(srv.ListenAndServe())
}
