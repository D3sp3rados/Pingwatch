package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// PgStore speichert alles in PostgreSQL:
//   - Rohwerte (jede Messung) für rawKeepDays Tage
//   - Minuten-Zusammenfassungen für die eingestellte Aufbewahrungszeit
//   - Ereignisse und Konfiguration
type PgStore struct {
	db       *sql.DB
	mu       sync.Mutex
	raw      []rawRow
	minutes  map[minKey]*Agg
	aggCache map[string][]Agg
	cacheMu  sync.Mutex
	lastErr  time.Time
}

type rawRow struct {
	host string
	s    Sample
}

type minKey struct {
	host string
	t    int64
}

const rawKeepDays = 3
const maxPending = 3_000_000

var pgSchema = []string{
	`CREATE TABLE IF NOT EXISTS pw_config (id INT PRIMARY KEY, data TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS pw_samples (host TEXT NOT NULL, t BIGINT NOT NULL, r INT NOT NULL, f SMALLINT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS pw_samples_host_t ON pw_samples (host, t)`,
	`CREATE INDEX IF NOT EXISTS pw_samples_t ON pw_samples (t)`,
	`CREATE TABLE IF NOT EXISTS pw_minutes (host TEXT NOT NULL, t BIGINT NOT NULL, n INT NOT NULL, lost INT NOT NULL, spk INT NOT NULL, sum BIGINT NOT NULL, mn INT NOT NULL, mx INT NOT NULL, PRIMARY KEY (host, t))`,
	`CREATE INDEX IF NOT EXISTS pw_minutes_t ON pw_minutes (t)`,
	`CREATE TABLE IF NOT EXISTS pw_events (id BIGINT PRIMARY KEY, start_t BIGINT NOT NULL, end_t BIGINT NOT NULL, data TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS pw_events_start ON pw_events (start_t)`,
}

// databaseDSN ermittelt die Verbindungsdaten aus der Umgebung.
// Unterstützt DATABASE_URL, DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/DB_NAME und die
// Standard-Variablen PGHOST, PGUSER usw.
func databaseDSN() (string, bool) {
	if u := strings.TrimSpace(os.Getenv("DATABASE_URL")); u != "" {
		return u, true
	}
	if h := os.Getenv("DB_HOST"); h != "" {
		u := &url.URL{Scheme: "postgres", Host: h, Path: "/" + envOr("DB_NAME", "pingwatch")}
		if p := os.Getenv("DB_PORT"); p != "" {
			u.Host = h + ":" + p
		}
		u.User = url.UserPassword(os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"))
		q := url.Values{}
		if m := os.Getenv("DB_SSLMODE"); m != "" {
			q.Set("sslmode", m)
		}
		u.RawQuery = q.Encode()
		return u.String(), true
	}
	if os.Getenv("PGHOST") != "" {
		return "", true
	}
	return "", false
}

func withSSLMode(dsn, mode string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set("sslmode", mode)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return strings.TrimSpace(dsn + " sslmode=" + mode)
}

func hasSSLMode(dsn string) bool {
	return strings.Contains(dsn, "sslmode=") || os.Getenv("PGSSLMODE") != ""
}

func openPg(dsn string) (*sql.DB, error) {
	try := func(d string) (*sql.DB, error) {
		db, err := sql.Open("postgres", d)
		if err != nil {
			return nil, err
		}
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	db, err := try(dsn)
	if err != nil && !hasSSLMode(dsn) && strings.Contains(strings.ToLower(err.Error()), "ssl is not enabled") {
		log.Printf("Datenbank ohne SSL – verbinde mit sslmode=disable")
		db, err = try(withSSLMode(dsn, "disable"))
	}
	return db, err
}

func NewPgStore(dsn string) (*PgStore, error) {
	var db *sql.DB
	var err error
	for i := 0; i < 20; i++ {
		db, err = openPg(dsn)
		if err == nil {
			break
		}
		log.Printf("Datenbank nicht erreichbar (%v) – neuer Versuch in 5 s", err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(3)
	db.SetConnMaxLifetime(30 * time.Minute)
	for _, q := range pgSchema {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("Tabellen anlegen: %v", err)
		}
	}
	return &PgStore{db: db, minutes: map[minKey]*Agg{}, aggCache: map[string][]Agg{}}, nil
}

func (s *PgStore) Kind() string { return "postgres" }

func (s *PgStore) Append(host string, sm Sample) {
	k := minKey{host, sm.T - sm.T%60_000}
	s.mu.Lock()
	if len(s.raw) < maxPending {
		s.raw = append(s.raw, rawRow{host, sm})
	}
	a := s.minutes[k]
	if a == nil {
		a = &Agg{}
		s.minutes[k] = a
	}
	a.Add(sm)
	s.mu.Unlock()
	if day := dayOf(sm.T); day != dayOf(time.Now().UnixMilli()) {
		s.cacheMu.Lock()
		delete(s.aggCache, host+"|"+day)
		s.cacheMu.Unlock()
	}
}

func (s *PgStore) logErr(what string, err error) {
	if time.Since(s.lastErr) > time.Minute {
		log.Printf("Datenbank: %s fehlgeschlagen: %v", what, err)
		s.lastErr = time.Now()
	}
}

// Flush schreibt gepufferte Messwerte. Bei Fehlern bleiben sie im Puffer.
func (s *PgStore) Flush() {
	s.mu.Lock()
	raw := s.raw
	mins := s.minutes
	s.raw = nil
	s.minutes = map[minKey]*Agg{}
	s.mu.Unlock()
	if len(raw) == 0 && len(mins) == 0 {
		return
	}
	if err := s.writeRaw(raw); err != nil {
		s.logErr("Rohwerte schreiben", err)
		s.mu.Lock()
		if len(s.raw)+len(raw) < maxPending {
			s.raw = append(raw, s.raw...)
		}
		s.mu.Unlock()
	}
	if err := s.writeMinutes(mins); err != nil {
		s.logErr("Minutenwerte schreiben", err)
		s.mu.Lock()
		for k, a := range mins {
			if b := s.minutes[k]; b != nil {
				a.Merge(*b)
			}
			s.minutes[k] = a
		}
		s.mu.Unlock()
	}
}

func (s *PgStore) writeRaw(raw []rawRow) error {
	if len(raw) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	st, err := tx.Prepare(pq.CopyIn("pw_samples", "host", "t", "r", "f"))
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, r := range raw {
		if _, err := st.Exec(r.host, r.s.T, r.s.R, int(r.s.F)); err != nil {
			st.Close()
			tx.Rollback()
			return err
		}
	}
	if _, err := st.Exec(); err != nil {
		st.Close()
		tx.Rollback()
		return err
	}
	st.Close()
	return tx.Commit()
}

const minuteUpsert = ` ON CONFLICT (host, t) DO UPDATE SET
  mn = CASE WHEN EXCLUDED.n - EXCLUDED.lost = 0 THEN pw_minutes.mn
            WHEN pw_minutes.n - pw_minutes.lost = 0 THEN EXCLUDED.mn
            ELSE LEAST(pw_minutes.mn, EXCLUDED.mn) END,
  mx = GREATEST(pw_minutes.mx, EXCLUDED.mx),
  n = pw_minutes.n + EXCLUDED.n,
  lost = pw_minutes.lost + EXCLUDED.lost,
  spk = pw_minutes.spk + EXCLUDED.spk,
  sum = pw_minutes.sum + EXCLUDED.sum`

func (s *PgStore) writeMinutes(mins map[minKey]*Agg) error {
	if len(mins) == 0 {
		return nil
	}
	keys := make([]minKey, 0, len(mins))
	for k := range mins {
		keys = append(keys, k)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for start := 0; start < len(keys); start += 500 {
		end := start + 500
		if end > len(keys) {
			end = len(keys)
		}
		var sb strings.Builder
		sb.WriteString("INSERT INTO pw_minutes (host, t, n, lost, spk, sum, mn, mx) VALUES ")
		args := make([]any, 0, (end-start)*8)
		for i, k := range keys[start:end] {
			a := mins[k]
			if i > 0 {
				sb.WriteByte(',')
			}
			p := len(args)
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)", p+1, p+2, p+3, p+4, p+5, p+6, p+7, p+8)
			args = append(args, k.host, k.t, a.N, a.Lost, a.Spk, a.Sum, a.Min, a.Max)
		}
		sb.WriteString(minuteUpsert)
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *PgStore) Close() {
	s.Flush()
	s.db.Close()
}

func dayRange(day string) (int64, int64) {
	d0 := dayStart(day)
	return d0.UnixMilli(), d0.AddDate(0, 0, 1).UnixMilli()
}

func (s *PgStore) ReadDay(host, day string) []Sample {
	a, b := dayRange(day)
	rows, err := s.db.Query(`SELECT t, r, f FROM pw_samples WHERE host = $1 AND t >= $2 AND t < $3 ORDER BY t`, host, a, b)
	if err != nil {
		s.logErr("Lesen", err)
		return nil
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var sm Sample
		var f int
		if rows.Scan(&sm.T, &sm.R, &f) == nil {
			sm.F = uint8(f)
			out = append(out, sm)
		}
	}
	return out
}

func (s *PgStore) DayAgg(host, day string) []Agg {
	key := host + "|" + day
	s.cacheMu.Lock()
	if a, ok := s.aggCache[key]; ok {
		s.cacheMu.Unlock()
		return a
	}
	s.cacheMu.Unlock()
	aggs := make([]Agg, 1440)
	a, b := dayRange(day)
	rows, err := s.db.Query(`SELECT t, n, lost, spk, sum, mn, mx FROM pw_minutes WHERE host = $1 AND t >= $2 AND t < $3`, host, a, b)
	if err != nil {
		s.logErr("Lesen", err)
		return aggs
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		var g Agg
		if rows.Scan(&t, &g.N, &g.Lost, &g.Spk, &g.Sum, &g.Min, &g.Max) != nil {
			continue
		}
		tt := time.UnixMilli(t)
		if idx := tt.Hour()*60 + tt.Minute(); idx >= 0 && idx < 1440 {
			aggs[idx].Merge(g)
		}
	}
	if day != dayOf(time.Now().UnixMilli()) {
		s.cacheMu.Lock()
		if len(s.aggCache) > 2000 {
			s.aggCache = map[string][]Agg{}
		}
		s.aggCache[key] = aggs
		s.cacheMu.Unlock()
	}
	return aggs
}

func (s *PgStore) Maintenance(retentionDays int) {
	now := time.Now()
	rawCut := now.AddDate(0, 0, -rawKeepDays).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM pw_samples WHERE t < $1`, rawCut); err != nil {
		s.logErr("Aufräumen", err)
	}
	if retentionDays > 0 {
		cut := now.AddDate(0, 0, -retentionDays).UnixMilli()
		s.db.Exec(`DELETE FROM pw_minutes WHERE t < $1`, cut)
		s.db.Exec(`DELETE FROM pw_events WHERE start_t < $1 AND end_t > 0`, cut)
	}
}

func (s *PgStore) AppendEvent(e Event) {
	b, _ := json.Marshal(e)
	_, err := s.db.Exec(`INSERT INTO pw_events (id, start_t, end_t, data) VALUES ($1,$2,$3,$4)
		ON CONFLICT (id) DO UPDATE SET start_t = EXCLUDED.start_t, end_t = EXCLUDED.end_t, data = EXCLUDED.data`,
		e.ID, e.Start, e.End, string(b))
	if err != nil {
		s.logErr("Ereignis speichern", err)
	}
}

func (s *PgStore) LoadEvents(retentionDays int) []*Event {
	cut := int64(0)
	if retentionDays > 0 {
		cut = time.Now().AddDate(0, 0, -retentionDays).UnixMilli()
	}
	rows, err := s.db.Query(`SELECT data FROM pw_events WHERE start_t >= $1 ORDER BY start_t`, cut)
	if err != nil {
		s.logErr("Ereignisse laden", err)
		return nil
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var d string
		var e Event
		if rows.Scan(&d) == nil && json.Unmarshal([]byte(d), &e) == nil {
			out = append(out, &e)
		}
	}
	return out
}

// RewriteEvents: Aufräumen erledigt Maintenance direkt in der Datenbank.
func (s *PgStore) RewriteEvents(evs []*Event) {}

func (s *PgStore) LoadConfig() ([]byte, error) {
	var d string
	err := s.db.QueryRow(`SELECT data FROM pw_config WHERE id = 1`).Scan(&d)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return []byte(d), err
}

func (s *PgStore) SaveConfig(b []byte) error {
	_, err := s.db.Exec(`INSERT INTO pw_config (id, data) VALUES (1, $1) ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data`, string(b))
	return err
}
