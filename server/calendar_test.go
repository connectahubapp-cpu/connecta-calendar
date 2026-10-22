package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRecurrenceDates(t *testing.T) {
	got := recurrenceDates("2026-10-05", "weekly", "2026-11-02", "2026-10-10", "2026-12-31")
	want := []string{"2026-10-12", "2026-10-19", "2026-10-26", "2026-11-02"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weekly: got %v want %v", got, want)
	}
	// 月末の毎月は GAS (JavaScript の setUTCMonth) と同じく繰り越す: 1/31 → 3/3
	if got := recurrenceDates("2026-01-31", "monthly", "", "2026-01-01", "2026-03-31"); !reflect.DeepEqual(got, []string{"2026-01-31", "2026-03-03"}) {
		t.Fatalf("monthly: %v", got)
	}
	if got := recurrenceDates("2026-10-05", "none", "", "", ""); !reflect.DeepEqual(got, []string{"2026-10-05"}) {
		t.Fatalf("none: %v", got)
	}
}

func sampleSnapshot() *Snapshot {
	return &Snapshot{
		Members: []SnapMember{{Mid: "M1", Name: "山田 太郎"}, {Mid: "M2", Name: "佐藤 花子"}},
		Rows:    []SnapRow{{Mid: "M1", Key: "a", Dept: "運営部", Role: "代表"}, {Mid: "M2", Key: "b", Dept: "広報部", Role: "メンバー"}},
		Depts:   []string{"運営部", "広報部"},
		Rank:    map[string]int{"代表": 5, "メンバー": 1},
		Entries: []Entry{
			{ID: "SE1", Mid: "M1", Kind: "school", SD: "2026-10-21", ED: "2026-10-23", SM: 0, EM: 1440, AllDay: true, Rec: "none"},
			{ID: "SE2", Mid: "M2", Kind: "private", SD: "2026-10-22", ED: "2026-10-22", SM: 1140, EM: 1260, Rec: "weekly", Until: "2026-11-05"},
			{ID: "SE3", Mid: "GONE", Kind: "private", SD: "2026-10-22", ED: "2026-10-22", SM: 0, EM: 1440},
		},
		Busy:     []BusyItem{{Mid: "M2", Ymd: "2026-10-24", S: 0, E: 1440, Src: "susp", Mode: "full"}},
		Requests: []LeaveRequest{{ID: "L1", Status: "pending", Mid: "M2", From: "2026-11-01", Until: "2026-11-10", Mode: "完全に休止"}},
	}
}

func TestPayload(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ApplySnapshot(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	d, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 14, 30, 0, 0, jst)
	p := BuildPayload(d, now)
	if p.Today != "2026-10-22" || p.WeekMon != "2026-10-19" || p.Months[2].Last != "2026-12-31" {
		t.Fatalf("dates: %+v", p)
	}
	if n := len(p.Items["M1"]); n != 3 {
		t.Fatalf("M1 school days = %d, want 3", n)
	}
	// 毎週の私用: 10/22, 10/29, 11/5
	for _, d := range []string{"2026-10-22", "2026-10-29", "2026-11-05"} {
		it := p.Items["M2"][d]
		if len(it) != 1 || it[0][0] != 1140 || it[0][2] != "private" {
			t.Fatalf("M2 %s = %v", d, it)
		}
	}
	if _, ok := p.Items["M2"]["2026-11-12"]; ok {
		t.Fatal("recur_until を超えて展開している")
	}
	if it := p.Items["M2"]["2026-10-24"]; len(it) != 1 || it[0][3] != "full" {
		t.Fatalf("busy: %v", it)
	}
	if _, ok := p.Items["GONE"]; ok {
		t.Fatal("在籍していない人の予定が出ている")
	}
	if len(p.Requests) != 1 || p.Requests[0].ID != "" {
		t.Fatalf("requests: %+v", p.Requests)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Fatal(err)
	}
}

func TestAddDeleteAndResync(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ApplySnapshot(sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	e := Entry{ID: "GO-1", Mid: "M1", Kind: "private", SD: "2026-10-30", ED: "2026-10-30", SM: 600, EM: 720, By: "M1"}
	if err := st.AddEntry(e); err != nil {
		t.Fatal(err)
	}
	ops, _ := st.PendingOps(10)
	if len(ops) != 1 || ops[0].Op != "upsert" {
		t.Fatalf("ops: %+v", ops)
	}
	var se SheetEntry
	_ = json.Unmarshal(ops[0].Entry, &se)
	if se.Start != "2026-10-30T10:00" || se.End != "2026-10-30T12:00" {
		t.Fatalf("書き戻しの時刻: %+v", se)
	}
	// 書き戻し済みの行がシートから戻ってきても、ここの版が残る (二重にならない)
	sn := sampleSnapshot()
	sn.Entries = append(sn.Entries, Entry{ID: "GO-1", Mid: "M1", Kind: "private", SD: "2026-10-30", ED: "2026-10-30", SM: 600, EM: 720})
	if err := st.ApplySnapshot(sn); err != nil {
		t.Fatal(err)
	}
	d, _ := st.Load()
	n := 0
	for _, x := range d.Entries {
		if x.ID == "GO-1" {
			n++
			if x.Source != "go" {
				t.Fatal("GO-1 がシートの版に置き換わった")
			}
		}
	}
	if n != 1 {
		t.Fatalf("GO-1 が %d 件", n)
	}
	// シートから来た予定は取り消せない
	if ok, _ := st.DeleteEntry("SE1"); ok {
		t.Fatal("シートの予定を取り消せてしまう")
	}
	if ok, _ := st.DeleteEntry("GO-1"); !ok {
		t.Fatal("GO-1 を取り消せない")
	}
	ops, _ = st.PendingOps(10)
	if len(ops) != 2 || ops[1].Op != "delete" {
		t.Fatalf("ops after delete: %+v", ops)
	}
	_ = st.MarkDone([]int64{ops[0].OpID, ops[1].OpID})
	if ops, _ = st.PendingOps(10); len(ops) != 0 {
		t.Fatalf("done にならない: %+v", ops)
	}
	d, _ = st.Load()
	p := BuildPayload(d, time.Date(2026, 10, 22, 9, 0, 0, 0, jst))
	if _, ok := p.Items["M1"]["2026-10-30"]; ok {
		t.Fatal("取り消した予定がまだ出ている")
	}
}

func TestParseHM(t *testing.T) {
	for in, want := range map[string]int{"09:00": 540, "19:30": 1170, "24:00": 1440} {
		if got, ok := parseHM(in); !ok || got != want {
			t.Fatalf("%s → %d %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "25:00", "aa:bb", "9"} {
		if _, ok := parseHM(bad); ok {
			t.Fatalf("%q should fail", bad)
		}
	}
}
