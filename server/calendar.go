package main

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

// ============================================================================
// 画面用データ (CalendarWeb の D と同じ形)。予定の展開は GAS の _calCollect_ と同じ決め方。
// ============================================================================

var jst = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Tokyo"); err == nil {
		return l
	}
	return time.FixedZone("JST", 9*3600)
}()

var ymdRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func validYmd(s string) bool {
	if !ymdRe.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
func parseYmd(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }
func fmtYmd(t time.Time) string   { return t.Format("2006-01-02") }
func addDays(s string, n int) string {
	return fmtYmd(parseYmd(s).AddDate(0, 0, n))
}
func daysBetween(a, b string) int   { return int(parseYmd(b).Sub(parseYmd(a)).Hours() / 24) }
func hhmm(min int) string           { return fmt.Sprintf("%02d:%02d", min/60, min%60) }
func todayJST(now time.Time) string { return now.In(jst).Format("2006-01-02") }

// 繰り返しの回の開始日 (GAS の _scheduleRecurrenceDates_ と同じ)
func recurrenceDates(start, rec, until, rangeStart, rangeEnd string) []string {
	const maxOcc = 200
	var out []string
	if !validYmd(start) {
		return out
	}
	cur := parseYmd(start)
	for guard := 0; guard < maxOcc*4 && len(out) < maxOcc; guard++ {
		y := fmtYmd(cur)
		if until != "" && y > until {
			break
		}
		if rangeEnd != "" && y > rangeEnd {
			break
		}
		if rangeStart == "" || y >= rangeStart {
			out = append(out, y)
		}
		switch rec {
		case "daily":
			cur = cur.AddDate(0, 0, 1)
		case "weekly":
			cur = cur.AddDate(0, 0, 7)
		case "biweekly":
			cur = cur.AddDate(0, 0, 14)
		case "monthly":
			cur = cur.AddDate(0, 1, 0)
		default:
			return out
		}
	}
	return out
}

type Month struct {
	Y     int    `json:"y"`
	M     int    `json:"m"`
	First string `json:"first"`
	Last  string `json:"last"`
	Label string `json:"label"`
}
type PRow struct {
	Mid   string `json:"mid"`
	Key   string `json:"key"`
	Dept  string `json:"dept"`
	Role  string `json:"role"`
	Multi int    `json:"multi"`
}
type PName struct {
	Name string `json:"name"`
	Kana string `json:"kana"`
}
type GoEntry struct {
	ID     string `json:"id"`
	Mid    string `json:"mid"`
	Kind   string `json:"kind"`
	SD     string `json:"sd"`
	ED     string `json:"ed"`
	SM     int    `json:"sm"`
	EM     int    `json:"em"`
	AllDay bool   `json:"allDay"`
	Note   string `json:"note"`
	By     string `json:"by"`
	At     string `json:"at"`
}

// Payload: 画面に渡すもの。items[mid][ymd] = [[s, e, src, mode?], ...]
type Payload struct {
	V         int                                   `json:"v"`
	Today     string                                `json:"today"`
	WeekMon   string                                `json:"weekMon"`
	Months    []Month                               `json:"months"`
	From      string                                `json:"from"`
	To        string                                `json:"to"`
	Rows      []PRow                                `json:"rows"`
	Names     map[string]PName                      `json:"names"`
	Depts     []string                              `json:"depts"`
	Rank      map[string]int                        `json:"rank"`
	Items     map[string]map[string][][]interface{} `json:"items"`
	Requests  []LeaveRequest                        `json:"requests"`
	GoEntries []GoEntry                             `json:"goEntries"`
	LastSync  string                                `json:"lastSync"`
	NowMin    int                                   `json:"nowMin"`
	Served    string                                `json:"served"`
}

func BuildPayload(d *Data, now time.Time) *Payload {
	today := todayJST(now)
	t := parseYmd(today)
	var months []Month
	labels := []string{"今月", "来月", "再来月"}
	for k := 0; k < 3; k++ {
		f := time.Date(t.Year(), t.Month()+time.Month(k), 1, 0, 0, 0, 0, time.UTC)
		l := f.AddDate(0, 1, -1)
		months = append(months, Month{Y: f.Year(), M: int(f.Month()), First: fmtYmd(f), Last: fmtYmd(l), Label: labels[k]})
	}
	weekMon := addDays(today, -((int(t.Weekday()) + 6) % 7))
	from := months[0].First
	if weekMon < from {
		from = weekMon
	}
	to := addDays(months[2].Last, 6)
	if a := addDays(today, 100); a > to {
		to = a
	}

	active := map[string]bool{}
	names := map[string]PName{}
	for _, m := range d.Members {
		active[m.Mid] = true
		names[m.Mid] = PName{Name: m.Name, Kana: m.Kana}
	}
	items := map[string]map[string][][]interface{}{}
	add := func(mid, ymd string, it []interface{}) {
		if !active[mid] || ymd < from || ymd > to {
			return
		}
		o := items[mid]
		if o == nil {
			o = map[string][][]interface{}{}
			items[mid] = o
		}
		o[ymd] = append(o[ymd], it)
	}
	// 学校・テスト / 私用 (繰り返しは期間内の回へ展開)
	for _, e := range d.Entries {
		if !active[e.Mid] || !validYmd(e.SD) {
			continue
		}
		type iv struct{ sd, ed string }
		ivs := []iv{{e.SD, orYmd(e.ED, e.SD)}}
		if r := normRec(e.Rec); r != "none" {
			dur := daysBetween(e.SD, ivs[0].ed)
			ivs = nil
			for _, od := range recurrenceDates(e.SD, r, e.Until, addDays(from, -(dur+1)), to) {
				ivs = append(ivs, iv{od, addDays(od, dur)})
			}
		}
		for _, v := range ivs {
			if v.ed < from || v.sd > to {
				continue
			}
			a, b := v.sd, v.ed
			if a < from {
				a = from
			}
			if b > to {
				b = to
			}
			for x, g := a, 0; x <= b && g < 400; x, g = addDays(x, 1), g+1 {
				s, en := 0, 1440 // その日の分 (GAS の _avdDayClip_)
				if x == v.sd {
					s = e.SM
				}
				if x == v.ed {
					en = e.EM
				}
				if en <= s {
					continue
				}
				add(e.Mid, x, []interface{}{s, en, normKind(e.Kind)})
			}
		}
	}
	// 休会・承認待ち・会議・イベント運営 (GAS が日ごとに展開済み)
	for _, b := range d.Busy {
		if b.Mode != "" {
			add(b.Mid, b.Ymd, []interface{}{b.S, b.E, b.Src, b.Mode})
		} else {
			add(b.Mid, b.Ymd, []interface{}{b.S, b.E, b.Src})
		}
	}
	rows := make([]PRow, 0, len(d.Rows))
	for _, r := range d.Rows {
		if active[r.Mid] {
			rows = append(rows, PRow{Mid: r.Mid, Key: r.Key, Dept: r.Dept, Role: r.Role, Multi: b2i(r.Multi)})
		}
	}
	reqs := make([]LeaveRequest, 0, len(d.Requests))
	for _, r := range d.Requests {
		if r.Until >= today && active[r.Mid] {
			r.ID = ""
			reqs = append(reqs, r)
		}
	}
	sort.SliceStable(reqs, func(i, j int) bool { // 承認待ちが先、そのあと始まりの早い順 (GAS 版と同じ)
		if (reqs[i].Status == "pending") != (reqs[j].Status == "pending") {
			return reqs[i].Status == "pending"
		}
		return reqs[i].From < reqs[j].From
	})
	var goes []GoEntry
	for _, e := range d.Entries {
		if e.Source == "go" && e.ED >= addDays(today, -7) {
			goes = append(goes, GoEntry{ID: e.ID, Mid: e.Mid, Kind: e.Kind, SD: e.SD, ED: e.ED, SM: e.SM, EM: e.EM, AllDay: e.AllDay, Note: e.Note, By: e.By, At: e.At})
		}
	}
	sort.Slice(goes, func(i, j int) bool { return goes[i].SD < goes[j].SD })
	depts := d.Depts
	if depts == nil {
		depts = []string{}
	}
	return &Payload{V: 1, Today: today, WeekMon: weekMon, Months: months, From: from, To: to, Rows: rows, Names: names, Depts: depts,
		Rank: d.Rank, Items: items, Requests: reqs, GoEntries: goes, LastSync: d.LastSync}
}
