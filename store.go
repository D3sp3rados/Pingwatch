package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample: eine Messung. R in Zehntel-ms (-1 = verloren), F: 0 ok, 1 Spitze, 2 verloren.
type Sample struct {
	T int64
	R int32
	F uint8
}

// Agg fasst Messungen eines Zeitabschnitts zusammen.
type Agg struct {
	N, Lost, Spk int32
	Sum          int64
	Min, Max     int32
}

func (a *Agg) Add(s Sample) {
	a.N++
	if s.R < 0 {
		a.Lost++
		return
	}
	if s.F == 1 {
		a.Spk++
	}
	a.Sum += int64(s.R)
	if a.N-a.Lost == 1 || s.R < a.Min {
		a.Min = s.R
	}
	if s.R > a.Max {
		a.Max = s.R
	}
}

func (a *Agg) Merge(b Agg) {
	if b.N == 0 {
		return
	}
	ga, gb := a.N-a.Lost, b.N-b.Lost
	if gb > 0 {
		if ga == 0 || b.Min < a.Min {
			a.Min = b.Min
		}
		if b.Max > a.Max {
			a.Max = b.Max
		}
	}
	a.N += b.N
	a.Lost += b.Lost
	a.Spk += b.Spk
	a.Sum += b.Sum
}

func (a Agg) Avg() int32 {
	g := a.N - a.Lost
	if g <= 0 {
		return -1
	}
	return int32(a.Sum / int64(g))
}

// JSON kompakt: [n, verloren, spitzen, avg, min, max] (Zeiten in Zehntel-ms)
func (a Agg) MarshalJSON() ([]byte, error) {
	if a.N == 0 {
		return []byte("0"), nil
	}
	mn, mx := a.Min, a.Max
	if a.N == a.Lost {
		mn, mx = -1, -1
	}
	return []byte(fmt.Sprintf("[%d,%d,%d,%d,%d,%d]", a.N, a.Lost, a.Spk, a.Avg(), mn, mx)), nil
}

func dayOf(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02") }

func dayStart(day string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", day, time.Local)
	return t
}

type dayFile struct {
	day string
	f   *os.File
	w   *bufio.Writer
}

type FileStore struct {
	dir      string
	mu       sync.Mutex
	writers  map[string]*dayFile
	aggCache map[string][]Agg
}

func NewFileStore(dir string) *FileStore {
	os.MkdirAll(filepath.Join(dir, "samples"), 0o755)
	return &FileStore{dir: dir, writers: map[string]*dayFile{}, aggCache: map[string][]Agg{}}
}

func (s *FileStore) hostDir(host string) string { return filepath.Join(s.dir, "samples", host) }

func fmtR(r int32) string {
	if r < 0 {
		return ""
	}
	return strconv.FormatFloat(float64(r)/10, 'f', 1, 64)
}

var flagChars = []byte{'o', 's', 'l'}

func (s *FileStore) Append(host string, sm Sample) {
	day := dayOf(sm.T)
	line := fmt.Sprintf("%d;%s;%c\n", sm.T, fmtR(sm.R), flagChars[sm.F])
	s.mu.Lock()
	defer s.mu.Unlock()
	df := s.writers[host]
	if df != nil && df.day == day {
		df.w.WriteString(line)
		return
	}
	today := dayOf(time.Now().UnixMilli())
	if day != today && df != nil && df.day == today {
		// verspäteter Messwert (z. B. vom Agenten nachgeliefert) für einen anderen Tag
		s.appendDirect(host, day, line)
		return
	}
	if df != nil {
		df.w.Flush()
		df.f.Close()
	}
	os.MkdirAll(s.hostDir(host), 0o755)
	f, err := os.OpenFile(filepath.Join(s.hostDir(host), day+".csv"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("Speichern fehlgeschlagen: %v", err)
		delete(s.writers, host)
		return
	}
	df = &dayFile{day: day, f: f, w: bufio.NewWriterSize(f, 64*1024)}
	s.writers[host] = df
	df.w.WriteString(line)
	delete(s.aggCache, host+"|"+day)
}

func (s *FileStore) appendDirect(host, day, line string) {
	os.MkdirAll(s.hostDir(host), 0o755)
	f, err := os.OpenFile(filepath.Join(s.hostDir(host), day+".csv"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line)
	f.Close()
	delete(s.aggCache, host+"|"+day)
}

func (s *FileStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, df := range s.writers {
		df.w.Flush()
	}
}

func (s *FileStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, df := range s.writers {
		df.w.Flush()
		df.f.Close()
		delete(s.writers, k)
	}
}

func parseSamples(r io.Reader, out []Sample) []Sample {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		i := bytes.IndexByte(b, ';')
		if i <= 0 {
			continue
		}
		j := bytes.IndexByte(b[i+1:], ';')
		if j < 0 {
			continue
		}
		j += i + 1
		t, err := strconv.ParseInt(string(b[:i]), 10, 64)
		if err != nil {
			continue
		}
		sm := Sample{T: t, R: -1, F: 2}
		if rs := b[i+1 : j]; len(rs) > 0 {
			v, err := strconv.ParseFloat(string(rs), 64)
			if err == nil {
				sm.R = int32(v*10 + 0.5)
				sm.F = 0
			}
		}
		if j+1 < len(b) && b[j+1] == 's' && sm.R >= 0 {
			sm.F = 1
		}
		out = append(out, sm)
	}
	return out
}

// ReadDay liest alle Messungen eines Tages (sortiert).
func (s *FileStore) ReadDay(host, day string) []Sample {
	var out []Sample
	base := filepath.Join(s.hostDir(host), day)
	if f, err := os.Open(base + ".csv.gz"); err == nil {
		if gz, err := gzip.NewReader(f); err == nil {
			out = parseSamples(gz, out)
			gz.Close()
		}
		f.Close()
	}
	if f, err := os.Open(base + ".csv"); err == nil {
		out = parseSamples(f, out)
		f.Close()
	}
	if !sort.SliceIsSorted(out, func(i, j int) bool { return out[i].T < out[j].T }) {
		sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	}
	return out
}

// DayAgg liefert 1440 Minuten-Aggregate eines vergangenen Tages (gecacht).
func (s *FileStore) DayAgg(host, day string) []Agg {
	key := host + "|" + day
	s.mu.Lock()
	if a, ok := s.aggCache[key]; ok {
		s.mu.Unlock()
		return a
	}
	if df := s.writers[host]; df != nil && df.day == day {
		df.w.Flush()
	}
	s.mu.Unlock()
	aggs := minuteAggs(s.ReadDay(host, day), day)
	s.mu.Lock()
	if len(s.aggCache) > 2000 {
		s.aggCache = map[string][]Agg{}
	}
	s.aggCache[key] = aggs
	s.mu.Unlock()
	return aggs
}

func minuteAggs(samples []Sample, day string) []Agg {
	aggs := make([]Agg, 1440)
	ds := dayStart(day)
	for _, sm := range samples {
		t := time.UnixMilli(sm.T)
		if t.Before(ds) {
			continue
		}
		idx := t.Hour()*60 + t.Minute()
		if idx >= 0 && idx < 1440 {
			aggs[idx].Add(sm)
		}
	}
	return aggs
}

// Maintenance: alte Tage komprimieren, abgelaufene löschen.
func (s *FileStore) Maintenance(retentionDays int) {
	s.Flush()
	root := filepath.Join(s.dir, "samples")
	hosts, _ := os.ReadDir(root)
	today := time.Now()
	yesterday := today.AddDate(0, 0, -1).Format("2006-01-02")
	cutoff := today.AddDate(0, 0, -retentionDays).Format("2006-01-02")
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		dir := filepath.Join(root, h.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			name := f.Name()
			day := strings.SplitN(name, ".", 2)[0]
			if len(day) != 10 {
				continue
			}
			if retentionDays > 0 && day < cutoff {
				os.Remove(filepath.Join(dir, name))
				continue
			}
			if strings.HasSuffix(name, ".csv") && !strings.HasSuffix(name, ".gz") && day < yesterday {
				s.compressDay(h.Name(), day)
			}
		}
	}
}

func (s *FileStore) compressDay(host, day string) {
	samples := s.ReadDay(host, day)
	base := filepath.Join(s.hostDir(host), day)
	tmp := base + ".csv.gz.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	gz := gzip.NewWriter(f)
	w := bufio.NewWriter(gz)
	for _, sm := range samples {
		fmt.Fprintf(w, "%d;%s;%c\n", sm.T, fmtR(sm.R), flagChars[sm.F])
	}
	w.Flush()
	gz.Close()
	f.Close()
	if err := os.Rename(tmp, base+".csv.gz"); err == nil {
		os.Remove(base + ".csv")
	}
}

// ---- Ereignisse ----

type Event struct {
	ID       int64  `json:"id"`
	HostID   string `json:"host"`
	HostName string `json:"name"`
	Site     string `json:"site"`
	Org      string `json:"org,omitempty"`
	Type     string `json:"type"` // outage, loss, spike, offline
	Start    int64  `json:"start"`
	End      int64  `json:"end"`  // 0 = läuft noch
	Last     int64  `json:"last"` // letzter auffälliger Messwert
	Count    int    `json:"count"`
	Max      int32  `json:"max"`
	Sum      int64  `json:"sum"`
	Dev      string `json:"dev,omitempty"`     // UniFi-Gerät (MAC) bei device/reboot/wan
	Cause    string `json:"cause,omitempty"`   // Ausfall wegen übergeordnetem Host
	Maint    bool   `json:"maint,omitempty"`   // während Wartung
	Trace    string `json:"trace,omitempty"`   // Routenverfolgung beim Ausfall
	AckBy    string `json:"ackBy,omitempty"`   // quittiert von
	AckAt    int64  `json:"ackAt,omitempty"`   // quittiert um
	AckNote  string `json:"ackNote,omitempty"` // Notiz beim Quittieren
	Notified bool   `json:"-"`                 // Beginn wurde gemeldet
	Decided  bool   `json:"-"`                 // Wartung/Ursache geprüft
}

func (s *FileStore) AppendEvent(e Event) {
	f, err := os.OpenFile(filepath.Join(s.dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	b, _ := json.Marshal(e)
	f.Write(append(b, '\n'))
	f.Close()
}

func (s *FileStore) LoadEvents(retentionDays int) []*Event {
	f, err := os.Open(filepath.Join(s.dir, "events.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	cut := time.Now().AddDate(0, 0, -retentionDays).UnixMilli()
	var out []*Event
	idx := map[int64]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && (retentionDays <= 0 || e.Start >= cut) {
			if i, ok := idx[e.ID]; ok {
				out[i] = &e // spätere Zeile (z. B. mit Traceroute) gewinnt
				continue
			}
			idx[e.ID] = len(out)
			out = append(out, &e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

func (s *FileStore) RewriteEvents(evs []*Event) {
	tmp := filepath.Join(s.dir, "events.jsonl.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range evs {
		if e.End == 0 {
			continue
		}
		b, _ := json.Marshal(e)
		w.Write(append(b, '\n'))
	}
	w.Flush()
	f.Close()
	os.Rename(tmp, filepath.Join(s.dir, "events.jsonl"))
}

// Storage ist die gemeinsame Schnittstelle für Datei- und Datenbank-Speicher.
type Storage interface {
	Append(host string, sm Sample)
	Flush()
	Close()
	ReadDay(host, day string) []Sample
	DayAgg(host, day string) []Agg
	Maintenance(retentionDays int)
	AppendEvent(e Event)
	LoadEvents(retentionDays int) []*Event
	RewriteEvents(evs []*Event)
	DeleteSamples(hosts []string, from, to int64)
	DeleteEvents(ids []int64)
	LoadConfig() ([]byte, error)
	SaveConfig(b []byte) error
	Kind() string
}

func (s *FileStore) LoadConfig() ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, "config.json"))
}

func (s *FileStore) SaveConfig(b []byte) error {
	p := filepath.Join(s.dir, "config.json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (s *FileStore) Kind() string { return "files" }

// DeleteSamples entfernt Messwerte im Zeitraum [from, to) (ms) für die angegebenen Hosts.
func (s *FileStore) DeleteSamples(hosts []string, from, to int64) {
	s.Close() // offene Tagesdateien schließen, werden beim nächsten Append neu geöffnet
	now := time.Now()
	for _, host := range hosts {
		end := time.UnixMilli(to)
		if end.After(now) {
			end = now
		}
		for d := time.UnixMilli(from); !dayStart(d.Format("2006-01-02")).After(end); d = d.AddDate(0, 0, 1) {
			day := d.Format("2006-01-02")
			base := filepath.Join(s.hostDir(host), day)
			_, e1 := os.Stat(base + ".csv")
			_, e2 := os.Stat(base + ".csv.gz")
			if e1 != nil && e2 != nil {
				continue
			}
			var keep []Sample
			for _, sm := range s.ReadDay(host, day) {
				if sm.T < from || sm.T >= to {
					keep = append(keep, sm)
				}
			}
			os.Remove(base + ".csv.gz")
			f, err := os.Create(base + ".csv.tmp")
			if err != nil {
				continue
			}
			w := bufio.NewWriter(f)
			for _, sm := range keep {
				fmt.Fprintf(w, "%d;%s;%c\n", sm.T, fmtR(sm.R), flagChars[sm.F])
			}
			w.Flush()
			f.Close()
			os.Rename(base+".csv.tmp", base+".csv")
			s.mu.Lock()
			delete(s.aggCache, host+"|"+day)
			s.mu.Unlock()
		}
	}
}

// DeleteEvents: bei Dateien schreibt RewriteEvents die bereinigte Liste.
func (s *FileStore) DeleteEvents(ids []int64) {}
