package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ============================================================================
// 保存 (SQLite)。シートから来たもの (source=sheet) と、ここで追加したもの (source=go) を同じ entries に持つ。
// ここで追加した予定は outbox に積み、GAS が schedule_entries へ書き戻したら done にする。
// ============================================================================

const schema = `
CREATE TABLE IF NOT EXISTS members (mid TEXT PRIMARY KEY, name TEXT NOT NULL, kana TEXT NOT NULL DEFAULT '', seq INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS member_rows (seq INTEGER PRIMARY KEY, mid TEXT NOT NULL, dkey TEXT NOT NULL, dept TEXT NOT NULL, dorder INTEGER NOT NULL, role TEXT NOT NULL, multi INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS depts (name TEXT PRIMARY KEY, seq INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS ranks (role TEXT PRIMARY KEY, rank INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS entries (
  id TEXT PRIMARY KEY, mid TEXT NOT NULL, kind TEXT NOT NULL,
  sd TEXT NOT NULL, ed TEXT NOT NULL, sm INTEGER NOT NULL, em INTEGER NOT NULL, all_day INTEGER NOT NULL,
  rec TEXT NOT NULL DEFAULT 'none', until TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS entries_mid ON entries(mid);
CREATE TABLE IF NOT EXISTS busy (mid TEXT NOT NULL, ymd TEXT NOT NULL, s INTEGER NOT NULL, e INTEGER NOT NULL, src TEXT NOT NULL, mode TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS busy_mid ON busy(mid);
CREATE TABLE IF NOT EXISTS requests (seq INTEGER PRIMARY KEY, rid TEXT NOT NULL, status TEXT NOT NULL, mid TEXT NOT NULL, from_d TEXT NOT NULL, until_d TEXT NOT NULL, mode TEXT NOT NULL, requested TEXT NOT NULL, via TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS outbox (id INTEGER PRIMARY KEY AUTOINCREMENT, op TEXT NOT NULL, entry_id TEXT NOT NULL, payload TEXT NOT NULL, created_at TEXT NOT NULL, done_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

type Store struct {
	db  *sql.DB
	mu  sync.Mutex // 書き込みは1本ずつ (SQLite)
	ver int64      // データの版。変わったら画面用データを作り直す
	vmu sync.RWMutex
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db, ver: time.Now().UnixNano()}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Version() int64 { s.vmu.RLock(); defer s.vmu.RUnlock(); return s.ver }
func (s *Store) bump()          { s.vmu.Lock(); s.ver++; s.vmu.Unlock() }

// ---- GAS から届く中身 ----
type Snapshot struct {
	SentAt   string         `json:"sentAt"`
	Today    string         `json:"today"`
	Members  []SnapMember   `json:"members"`
	Rows     []SnapRow      `json:"rows"`
	Depts    []string       `json:"depts"`
	Rank     map[string]int `json:"rank"`
	Entries  []Entry        `json:"entries"`
	Busy     []BusyItem     `json:"busy"`
	Requests []LeaveRequest `json:"requests"`
}
type SnapMember struct {
	Mid  string `json:"mid"`
	Name string `json:"name"`
	Kana string `json:"kana"`
}
type SnapRow struct {
	Mid   string `json:"mid"`
	Key   string `json:"key"`
	Dept  string `json:"dept"`
	Order int    `json:"order"`
	Role  string `json:"role"`
	Multi bool   `json:"multi"`
}
type Entry struct {
	ID     string `json:"id"`
	Mid    string `json:"mid"`
	Kind   string `json:"kind"` // school | private
	SD     string `json:"sd"`
	ED     string `json:"ed"`
	SM     int    `json:"sm"`
	EM     int    `json:"em"`
	AllDay bool   `json:"allDay"`
	Rec    string `json:"rec"`
	Until  string `json:"until"`
	By     string `json:"by"`
	Note   string `json:"note,omitempty"`
	Source string `json:"source,omitempty"`
	At     string `json:"at,omitempty"`
}
type BusyItem struct {
	Mid  string `json:"mid"`
	Ymd  string `json:"ymd"`
	S    int    `json:"s"`
	E    int    `json:"e"`
	Src  string `json:"src"`
	Mode string `json:"mode"`
}
type LeaveRequest struct {
	ID        string `json:"id,omitempty"`
	Status    string `json:"status"`
	Mid       string `json:"mid"`
	From      string `json:"from"`
	Until     string `json:"until"`
	Mode      string `json:"mode"`
	Requested string `json:"requested"`
	Via       string `json:"via"`
}

// ApplySnapshot: シート由来のものを丸ごと入れ替える。ここで追加した予定 (source=go) は残す。
func (s *Store) ApplySnapshot(sn *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM members", "DELETE FROM member_rows", "DELETE FROM depts", "DELETE FROM ranks", "DELETE FROM busy", "DELETE FROM requests", "DELETE FROM entries WHERE source='sheet'"} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	ins := func(q string, rows func(st *sql.Stmt) error) error {
		st, err := tx.Prepare(q)
		if err != nil {
			return err
		}
		defer st.Close()
		return rows(st)
	}
	if err := ins("INSERT OR REPLACE INTO members(mid,name,kana,seq) VALUES(?,?,?,?)", func(st *sql.Stmt) error {
		for i, m := range sn.Members {
			if _, err := st.Exec(m.Mid, m.Name, m.Kana, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ins("INSERT INTO member_rows(seq,mid,dkey,dept,dorder,role,multi) VALUES(?,?,?,?,?,?,?)", func(st *sql.Stmt) error {
		for i, r := range sn.Rows {
			if _, err := st.Exec(i, r.Mid, r.Key, r.Dept, r.Order, r.Role, b2i(r.Multi)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ins("INSERT OR REPLACE INTO depts(name,seq) VALUES(?,?)", func(st *sql.Stmt) error {
		for i, d := range sn.Depts {
			if _, err := st.Exec(d, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ins("INSERT OR REPLACE INTO ranks(role,rank) VALUES(?,?)", func(st *sql.Stmt) error {
		for k, v := range sn.Rank {
			if _, err := st.Exec(k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// 同じ id がここで追加したもの (source=go) にあれば、そちらを正とする (書き戻し済みの行が戻ってきただけ)
	if err := ins(`INSERT OR IGNORE INTO entries(id,mid,kind,sd,ed,sm,em,all_day,rec,until,source,created_by) VALUES(?,?,?,?,?,?,?,?,?,?,'sheet',?)`, func(st *sql.Stmt) error {
		for _, e := range sn.Entries {
			if e.ID == "" || !validYmd(e.SD) {
				continue
			}
			if _, err := st.Exec(e.ID, e.Mid, normKind(e.Kind), e.SD, orYmd(e.ED, e.SD), e.SM, e.EM, b2i(e.AllDay), normRec(e.Rec), e.Until, e.By); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ins("INSERT INTO busy(mid,ymd,s,e,src,mode) VALUES(?,?,?,?,?,?)", func(st *sql.Stmt) error {
		for _, b := range sn.Busy {
			if _, err := st.Exec(b.Mid, b.Ymd, b.S, b.E, b.Src, b.Mode); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ins("INSERT INTO requests(seq,rid,status,mid,from_d,until_d,mode,requested,via) VALUES(?,?,?,?,?,?,?,?,?)", func(st *sql.Stmt) error {
		for i, r := range sn.Requests {
			if _, err := st.Exec(i, r.ID, r.Status, r.Mid, r.From, r.Until, r.Mode, r.Requested, r.Via); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta(k,v) VALUES('last_sync',?)", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bump()
	return nil
}

// ---- 読み出し (画面用データの材料) ----
type Data struct {
	Members  []SnapMember
	Rows     []SnapRow
	Depts    []string
	Rank     map[string]int
	Entries  []Entry
	Busy     []BusyItem
	Requests []LeaveRequest
	LastSync string
}

func (s *Store) Load() (*Data, error) {
	d := &Data{Rank: map[string]int{}}
	q := func(query string, scan func(*sql.Rows) error) error {
		rs, err := s.db.Query(query)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			if err := scan(rs); err != nil {
				return err
			}
		}
		return rs.Err()
	}
	if err := q("SELECT mid,name,kana FROM members ORDER BY seq", func(r *sql.Rows) error {
		var m SnapMember
		err := r.Scan(&m.Mid, &m.Name, &m.Kana)
		d.Members = append(d.Members, m)
		return err
	}); err != nil {
		return nil, err
	}
	if err := q("SELECT mid,dkey,dept,dorder,role,multi FROM member_rows ORDER BY seq", func(r *sql.Rows) error {
		var x SnapRow
		var multi int
		err := r.Scan(&x.Mid, &x.Key, &x.Dept, &x.Order, &x.Role, &multi)
		x.Multi = multi == 1
		d.Rows = append(d.Rows, x)
		return err
	}); err != nil {
		return nil, err
	}
	if err := q("SELECT name FROM depts ORDER BY seq", func(r *sql.Rows) error {
		var n string
		err := r.Scan(&n)
		d.Depts = append(d.Depts, n)
		return err
	}); err != nil {
		return nil, err
	}
	if err := q("SELECT role,rank FROM ranks", func(r *sql.Rows) error {
		var k string
		var v int
		err := r.Scan(&k, &v)
		d.Rank[k] = v
		return err
	}); err != nil {
		return nil, err
	}
	if err := q("SELECT id,mid,kind,sd,ed,sm,em,all_day,rec,until,source,note,created_by,created_at FROM entries WHERE deleted=0", func(r *sql.Rows) error {
		var e Entry
		var ad int
		err := r.Scan(&e.ID, &e.Mid, &e.Kind, &e.SD, &e.ED, &e.SM, &e.EM, &ad, &e.Rec, &e.Until, &e.Source, &e.Note, &e.By, &e.At)
		e.AllDay = ad == 1
		d.Entries = append(d.Entries, e)
		return err
	}); err != nil {
		return nil, err
	}
	if err := q("SELECT mid,ymd,s,e,src,mode FROM busy", func(r *sql.Rows) error {
		var b BusyItem
		err := r.Scan(&b.Mid, &b.Ymd, &b.S, &b.E, &b.Src, &b.Mode)
		d.Busy = append(d.Busy, b)
		return err
	}); err != nil {
		return nil, err
	}
	if err := q("SELECT rid,status,mid,from_d,until_d,mode,requested,via FROM requests ORDER BY seq", func(r *sql.Rows) error {
		var x LeaveRequest
		err := r.Scan(&x.ID, &x.Status, &x.Mid, &x.From, &x.Until, &x.Mode, &x.Requested, &x.Via)
		d.Requests = append(d.Requests, x)
		return err
	}); err != nil {
		return nil, err
	}
	_ = s.db.QueryRow("SELECT v FROM meta WHERE k='last_sync'").Scan(&d.LastSync)
	return d, nil
}

func (s *Store) MemberExists(mid string) bool {
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM members WHERE mid=?", mid).Scan(&n)
	return n > 0
}

// ---- ここで追加・取り消し (outbox に積む) ----
type OutOp struct {
	OpID  int64           `json:"opId"`
	Op    string          `json:"op"` // upsert | delete
	Entry json.RawMessage `json:"entry"`
}

// 書き戻し用の予定の形 (GAS が schedule_entries にそのまま書く)
type SheetEntry struct {
	ID     string `json:"id"`
	Mid    string `json:"mid"`
	Kind   string `json:"kind"`
	Start  string `json:"start"`
	End    string `json:"end"`
	AllDay bool   `json:"allDay"`
	Note   string `json:"note"`
	By     string `json:"by"`
}

func toSheetEntry(e Entry) SheetEntry {
	se := SheetEntry{ID: e.ID, Mid: e.Mid, Kind: e.Kind, AllDay: e.AllDay, Note: e.Note, By: e.By, Start: e.SD, End: e.ED}
	if !e.AllDay {
		se.Start = e.SD + "T" + hhmm(e.SM)
		em := e.EM
		if em >= 1440 {
			em = 1439
		}
		se.End = e.ED + "T" + hhmm(em)
	}
	return se
}

func (s *Store) AddEntry(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT INTO entries(id,mid,kind,sd,ed,sm,em,all_day,rec,until,source,note,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,'none','','go',?,?,?)`,
		e.ID, e.Mid, e.Kind, e.SD, e.ED, e.SM, e.EM, b2i(e.AllDay), e.Note, e.By, now); err != nil {
		return err
	}
	pl, _ := json.Marshal(toSheetEntry(e))
	if _, err := tx.Exec("INSERT INTO outbox(op,entry_id,payload,created_at) VALUES('upsert',?,?,?)", e.ID, string(pl), now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bump()
	return nil
}

// DeleteEntry: ここで追加した予定だけ取り消せる
func (s *Store) DeleteEntry(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE entries SET deleted=1 WHERE id=? AND source='go' AND deleted=0", id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	pl, _ := json.Marshal(SheetEntry{ID: id})
	if _, err := tx.Exec("INSERT INTO outbox(op,entry_id,payload,created_at) VALUES('delete',?,?,?)", id, string(pl), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.bump()
	return true, nil
}

func (s *Store) PendingOps(limit int) ([]OutOp, error) {
	rs, err := s.db.Query("SELECT id,op,payload FROM outbox WHERE done_at='' ORDER BY id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []OutOp
	for rs.Next() {
		var o OutOp
		var pl string
		if err := rs.Scan(&o.OpID, &o.Op, &pl); err != nil {
			return nil, err
		}
		o.Entry = json.RawMessage(pl)
		out = append(out, o)
	}
	return out, rs.Err()
}

func (s *Store) MarkDone(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range ids {
		if _, err := s.db.Exec("UPDATE outbox SET done_at=? WHERE id=? AND done_at=''", now, id); err != nil {
			return err
		}
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
func orYmd(a, b string) string {
	if validYmd(a) && a >= b {
		return a
	}
	return b
}
func normKind(k string) string {
	if k == "school" {
		return "school"
	}
	return "private"
}
func normRec(r string) string {
	switch r {
	case "daily", "weekly", "biweekly", "monthly":
		return r
	}
	return "none"
}
