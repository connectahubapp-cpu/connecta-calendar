# connecta-calendar（空き状況）

connec+a のメンバーの予定（学校・テスト／私用／休会）を見て、空いている日を探し、会議の候補を出す画面です。

```
画面 (GitHub Pages: docs/) ──▶ Go (Fly.io: server/) ── SQLite (/data/calendar.db)
                                   ▲                │
     GAS がシートの中身を送る (5分ごと) ┘                └▶ ここで追加・取り消した予定を GAS が schedule_entries に書き戻す
```

| 担当 | やること |
|---|---|
| `docs/` (GitHub Pages) | 画面。ログイン・今日・カレンダー・一覧・さがす（空き検索／日程調整）・申請・予定の追加 |
| `server/` (Go, Fly.io) | ログインの確認、画面用データの計算（繰り返し予定の展開など）、予定の追加・取り消しと保存、書き戻しの待ち行列 |
| GAS (`connecta-gas/backend/CalendarSync.js`) | データが変わったらシートの中身を Go へ送る／書き戻し／ログインは既存の `authenticate`・`whoami` |

- ログインは connec+a Console と同じ ID・パスワード。マネージャー以上だけが使えます（`MIN_RANK`）。
- 理由・メール・連絡先は Go に送りません。退会の申請も出しません。
- ここで追加した予定の id は `GO-` で始まります。GAS は `GO-` の行しか書き換えません。

## API

| | |
|---|---|
| `POST /api/login` | `{identifier, password}` → `{memberId, token, name}` |
| `GET /api/calendar` | 画面用データ（要ログイン: `Authorization: Bearer <token>` と `X-Member-Id`） |
| `POST /api/entries` | 予定の追加 `{mid, kind: school\|private, start, end, allDay, startTime, endTime, note}` |
| `DELETE /api/entries/{id}` | ここで追加した予定の取り消し |
| `POST /api/sync` | GAS から（`X-Sync-Secret`）。シートの中身を受け取り、書き戻し待ちを返す |

## 初回の手順

1. **Fly.io**（`server/` で）
   ```sh
   fly auth login
   fly launch --no-deploy --copy-config --name connecta-calendar --region nrt
   fly volumes create data --region nrt --size 1
   fly secrets set GAS_URL="https://script.google.com/macros/s/…/exec" SYNC_SECRET="<長い文字列>"
   fly deploy
   ```
   以後のデプロイも `server/` で `fly deploy`。push で自動にしたいときは `server/ci/github-actions-server.yml.example` を `.github/workflows/` に置き、
   `fly tokens create deploy` のトークンを GitHub の Secrets `FLY_API_TOKEN` に入れる (push には gh の `workflow` 権限が要る)。
2. **GitHub Pages**: Settings → Pages → 「Deploy from a branch」→ `main` / `/docs`。`docs/config.js` の `api` を Fly の URL に合わせる。
   公開先: https://connectahubapp-cpu.github.io/connecta-calendar/
3. **GAS**: スクリプトのプロパティに `CALSYNC_GO_URL`（Fly の URL）と `CALSYNC_SECRET`（`SYNC_SECRET` と同じ）を入れて `clasp push`、`calendarSyncNow` を1回実行。以後は leanSweep（5分ごと）が送ります。
   書き戻しは本番ウェブアプリの `doPost` を通るので、**ウェブアプリの新しいバージョンのデプロイ**も必要です。

## 手元で動かす

```sh
cd server
go test ./...
PORT=8080 DB_PATH=dev.db GAS_URL=… SYNC_SECRET=dev ALLOWED_ORIGIN=http://localhost:8791 go run .
```
`docs/` は静的ファイルなので `python -m http.server 8791` などで開けます（`config.js` の `api` を `http://localhost:8080` に）。
