// ============================================================================
// (2026-10-22) 空き状況カレンダー (GitHub Pages + Go) との連携 — GAS の担当はここだけ
// ============================================================================
//  ■ 役割分担
//    Go (Fly.io)   : 画面のデータを作る・予定の追加と取り消し・保存 (SQLite)。画面は GitHub Pages。
//    GAS (この file): ① データが変わったら、シートの中身 (名簿・予定・休会・会議など) を Go へ送る
//                     ② Go で追加・取り消した予定を schedule_entries に書き戻す (Console・フォームと同じ表に入る)
//                     ③ ログインの確認は既存の authenticate / whoami をそのまま使う (変更なし)
//  ■ 動くタイミング: leanSweep_ (5分ごと) の末尾の calendarSyncTick_。
//    data_gen か日付が変わった・30分たった → 全部を送る。それ以外 → 書き戻し待ちが無いかだけ聞く (軽い)。
//    Go は予定を追加した直後にも doPost (calendarSync.apply) で書き戻しを頼む。どちらで届いても結果は同じ (id で上書き)。
//  ■ 初回: スクリプトのプロパティに CALSYNC_GO_URL (例 https://connecta-calendar.fly.dev) と
//    CALSYNC_SECRET (Go の SYNC_SECRET と同じ長い文字列) を入れ、calendarSyncNow を1回実行。
//  ■ 理由・メール・連絡先は送らない。
// ============================================================================

var CALSYNC_URL_PROP_ = 'CALSYNC_GO_URL';
var CALSYNC_SECRET_PROP_ = 'CALSYNC_SECRET';
var CALSYNC_STATE_PROP_ = 'CALSYNC_LAST';     // { gen, day, at }
var CALSYNC_ACK_PROP_ = 'CALSYNC_ACK';         // 書き戻した op の id (次に Go へ伝える)
var CALSYNC_FULL_MS_ = 30 * 60 * 1000;
var CALSYNC_BACK_DAYS_ = 40;    // 送る期間: 今日の40日前〜
var CALSYNC_AHEAD_DAYS_ = 130;  //            〜130日後 (再来月の末 + 空き検索の92日を含む)

function _calSyncCfg_() {
  var p = PropertiesService.getScriptProperties();
  return { url: String(p.getProperty(CALSYNC_URL_PROP_) || '').replace(/\/+$/, ''), secret: String(p.getProperty(CALSYNC_SECRET_PROP_) || '') };
}

// ---- 5分ごと ----
function calendarSyncTick_() {
  var cfg = _calSyncCfg_();
  if (!cfg.url || !cfg.secret) return { skipped: true };
  var props = PropertiesService.getScriptProperties();
  var gen = String(CacheService.getScriptCache().get('data_gen') || '0'), day = _avdToday_();
  var last = {};
  try { last = JSON.parse(props.getProperty(CALSYNC_STATE_PROP_) || '{}') || {}; } catch (e) { last = {}; }
  var full = last.gen !== gen || last.day !== day || !last.at || Date.now() - last.at > CALSYNC_FULL_MS_;
  var r = _calSyncSend_(cfg, full);
  if (full && r.ok) props.setProperty(CALSYNC_STATE_PROP_, JSON.stringify({ gen: gen, day: day, at: Date.now() }));
  return r;
}
// GAS エディタから手で (初回・確認用)
function calendarSyncNow() {
  var cfg = _calSyncCfg_();
  if (!cfg.url || !cfg.secret) throw new Error('スクリプトのプロパティに ' + CALSYNC_URL_PROP_ + ' と ' + CALSYNC_SECRET_PROP_ + ' を入れてください');
  var r = _calSyncSend_(cfg, true);
  Logger.log(JSON.stringify(r));
  return r;
}

// 送る → 返ってきた書き戻しを適用 → 適用した id は次回 Go へ伝える
function _calSyncSend_(cfg, full) {
  var props = PropertiesService.getScriptProperties();
  var acked = [];
  try { acked = JSON.parse(props.getProperty(CALSYNC_ACK_PROP_) || '[]') || []; } catch (e) { acked = []; }
  var body = { applied: acked };
  if (full) body.snapshot = _calSyncSnapshot_();
  var t0 = Date.now();
  var res = UrlFetchApp.fetch(cfg.url + '/api/sync', {
    method: 'post', contentType: 'application/json', muteHttpExceptions: true,
    headers: { 'X-Sync-Secret': cfg.secret }, payload: JSON.stringify(body),
  });
  var code = res.getResponseCode();
  if (code !== 200) return { ok: false, error: 'HTTP ' + code + ' ' + String(res.getContentText() || '').slice(0, 120) };
  var out = {};
  try { out = JSON.parse(res.getContentText() || '{}') || {}; } catch (e) { out = {}; }
  var applied = _calSyncApplyOps_(out.ops || []);
  props.setProperty(CALSYNC_ACK_PROP_, JSON.stringify(applied));   // 前回の分は Go が受け取ったので入れ替える
  return { ok: true, full: full, ops: (out.ops || []).length, ms: Date.now() - t0 };
}

// ---- Go へ送る中身 ----
function _calSyncSnapshot_() {
  var today = _avdToday_();
  var from = _scheduleAddDaysStr_(today, -CALSYNC_BACK_DAYS_), to = _scheduleAddDaysStr_(today, CALSYNC_AHEAD_DAYS_);
  var R = _calRoster_();
  var members = R.people.map(function (r) { return { mid: r.mid, name: r.name, kana: r.kana }; });
  var rows = R.rows.map(function (r) { return { mid: r.mid, key: r.key, dept: r.dept, order: r.order, role: r.role, multi: !!r.multi }; });
  // 学校・テスト / 私用 は元の行のまま送る (繰り返しの展開は Go がする)
  var entries = [];
  (readSheetAsObjects('schedule_entries') || []).forEach(function (r) {
    var mid = String(r['member_id'] || ''); if (!R.dm.active[mid]) return;
    var kind = String(r['kind'] || 'personal');
    if (kind === 'availability' || kind === 'meeting_proposal') return;
    var iv = _avdEntryInterval_(r); if (!iv) return;
    entries.push({
      id: String(r['id'] || ''), mid: mid, kind: kind === 'school' ? 'school' : 'private',
      sd: iv.sd, ed: iv.ed, sm: iv.sm, em: iv.em, allDay: String(r['all_day'] || '').toUpperCase() === 'TRUE',
      rec: _scheduleNormRecurrence_(r['recurrence']), until: _scheduleNormDate_(r['recur_until']),
      by: String(r['created_by'] || ''),
    });
  });
  // 休会・承認待ち・会議・イベント運営は、日ごとに展開したものを送る (決め方は CalendarView.js と同じ)
  var busy = [];
  var raw = _calCollect_(R, from, to, true);
  Object.keys(raw).forEach(function (mid) {
    Object.keys(raw[mid]).forEach(function (ymd) {
      raw[mid][ymd].forEach(function (it) {
        if (it.src === 'school' || it.src === 'private') return;
        busy.push({ mid: mid, ymd: ymd, s: it.s, e: it.e, src: it.src, mode: it.mode || '' });
      });
    });
  });
  var requests = [];
  (readSheetAsObjects('leave_requests') || []).forEach(function (r) {
    if (!_suspIsLeaveRow_(r) || String(r['request_type'] || 'leave') === 'return_change') return;
    var st = String(r['status'] || '');
    if (st !== 'pending' && st !== 'approved') return;
    var mid = String(r['member_id'] || ''); if (!R.dm.active[mid]) return;
    var rg = _suspRowRange_(r); if (!rg || rg.until < rg.from || rg.until < today) return;
    requests.push({
      id: String(r['id'] || ''), status: st, mid: mid, from: rg.from, until: rg.until, mode: SUSP_MODES[String(r['activity_mode'])] || '',
      requested: _avdYmd_(r['requested_at'] || r['created_at']) || '',
      via: String(r['notes'] || '').indexOf('フォーム') >= 0 ? 'フォーム' : 'アプリ',
    });
  });
  return { sentAt: new Date().toISOString(), today: today, from: from, to: to, members: members, rows: rows, depts: R.depts, rank: ROLE_RANK_GAS, entries: entries, busy: busy, requests: requests };
}

// ---- 書き戻し (Go で追加・取り消した予定を schedule_entries へ) ----
//   ops: [{ opId, op: 'upsert' | 'delete', entry: { id, mid, kind, start, end, allDay, note, by } }]
function _calSyncApplyOps_(ops) {
  if (!ops || !ops.length) return [];
  var applied = [];
  var lock = LockService.getScriptLock(); lock.waitLock(20000);
  try {
    var sh = getSpreadsheet_().getSheetByName('schedule_entries');
    if (!sh) throw new Error('schedule_entries シートがありません');
    var lastCol = Math.max(1, sh.getLastColumn());
    var hdr = sh.getRange(1, 1, 1, lastCol).getValues()[0] || [];
    var cId = hdr.indexOf('id');
    if (cId < 0 || hdr.indexOf('member_id') < 0) throw new Error('schedule_entries の見出しが読めません');
    var lastRow = sh.getLastRow();
    var ids = lastRow >= 2 ? sh.getRange(2, cId + 1, lastRow - 1, 1).getValues().map(function (x) { return String(x[0]); }) : [];
    var touched = [], days = [];
    var deleteRows = [];
    ops.forEach(function (o) {
      var en = o.entry || {}, id = String(en.id || '');
      if (!id || id.indexOf('GO-') !== 0) { applied.push(o.opId); return; }   // Go で作ったもの以外は触らない
      var at = ids.indexOf(id);
      if (o.op === 'delete') {
        if (at >= 0) deleteRows.push(at + 2);
      } else {
        var kind = en.kind === 'school' ? 'school' : 'private';
        var now = new Date();
        var rec = {
          id: id, member_id: String(en.mid || ''), title: kind === 'school' ? '学校・テスト' : '私用', kind: kind,
          start: String(en.start || ''), end: String(en.end || ''), all_day: en.allDay ? 'TRUE' : 'FALSE', dept: '',
          visibility: 'self', note: String(en.note || '').slice(0, 200), created_by: String(en.by || ''), updated_at: now,
          recurrence: 'none', recur_until: '', reminder: 'none',
        };
        if (at >= 0) {
          var row = sh.getRange(at + 2, 1, 1, lastCol).getValues()[0];
          hdr.forEach(function (h, i) { if (rec.hasOwnProperty(String(h))) row[i] = rec[String(h)]; });
          sh.getRange(at + 2, 1, 1, lastCol).setValues([row]);
        } else {
          rec.created_at = now; rec.reminded_at = '';
          sh.appendRow(hdr.map(function (h) { return rec.hasOwnProperty(String(h)) ? rec[String(h)] : ''; }));
          ids.push(id);
        }
        touched.push(rec.member_id);
        try { days = days.concat(_avdDatesOf_(_avdEntryInterval_(rec))); } catch (e) {}
      }
      applied.push(o.opId);
    });
    deleteRows.sort(function (a, b) { return b - a; }).forEach(function (r) { sh.deleteRow(r); });
    if (touched.length || deleteRows.length) invalidateSheetCache_('schedule_entries');
    if (touched.length) { try { avdTouch_(touched, days); } catch (e) {} }
  } finally { try { lock.releaseLock(); } catch (e) {} }
  return applied;
}

// doPost から (Code.js の doPost の先頭で body.action === 'calendarSync.apply' のとき)。合言葉が合うときだけ
function calSyncDoPost_(body) {
  var cfg = _calSyncCfg_();
  var out;
  if (!cfg.secret || String(body.secret || '') !== cfg.secret) out = { ok: false, error: 'forbidden' };
  else {
    try { out = { ok: true, applied: _calSyncApplyOps_(body.ops || []) }; }
    catch (e) { out = { ok: false, error: String(e && e.message || e) }; }
  }
  return ContentService.createTextOutput(JSON.stringify(out)).setMimeType(ContentService.MimeType.JSON);
}
