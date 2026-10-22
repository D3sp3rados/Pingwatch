package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

// Sicherung und Wiederherstellung der kompletten Datenbank (Konfiguration, Ereignisse, Messwerte,
// Inventar, SNMP-Verlauf) – für Datei- und Postgres-Speicher.
//
// Format: gzip-komprimierte JSON-Zeilen (ein Datensatz je Zeile):
//   {"t":"meta",...}  Kopf mit Format, Version, Zeitpunkt, Speicherart
//   {"t":"config"}    komplette Konfiguration (Kunden, Benutzer inkl. Hashes, Hosts, Standorte inkl. Tokens …)
//   {"t":"event"}     ein Ereignis
//   {"t":"blob"}      Inventar u. Ä. (Schlüssel + Inhalt)
//   {"t":"file"}      Datei-Speicher: Messwert-Dateien (relativer Pfad + Inhalt)
//   {"t":"rows"}      Postgres: Zeilen einer Tabelle (Messwerte, Minutenwerte, SNMP)
//   {"t":"end"}       Abschluss mit Anzahlen – fehlt er, ist die Datei unvollständig
// Mit BACKUP_KEY wird der gzip-Strom zusätzlich mit AES-256-GCM verschlüsselt (Blöcke à 64 KiB).

const (
	backupFormat  = "pingwacht-backup"
	backupVersion = 1
	backupMagic   = "PWBK1\n"
	backupChunk   = 64 << 10
	backupKDFIter = 200_000
)

// storeFrozen: nach dem Start einer Wiederherstellung schreibt der laufende Prozess nichts mehr
// (sonst würde er die wiederhergestellten Daten mit dem alten Stand überschreiben).
var storeFrozen atomic.Bool

// restartFn beendet den Prozess nach einer Wiederherstellung (Container-Neustart lädt die neuen Daten).
var restartFn = func() {
	log.Printf("Wiederherstellung abgeschlossen – PingWacht startet neu")
	os.Exit(0)
}

type backupRec struct {
	T string `json:"t"`
	// meta
	Format string `json:"format,omitempty"`
	V      int    `json:"v,omitempty"`
	App    string `json:"app,omitempty"`
	At     int64  `json:"at,omitempty"`
	Store  string `json:"store,omitempty"`
	// config / event
	Data json.RawMessage `json:"data,omitempty"`
	// blob / file
	K string `json:"k,omitempty"`
	P string `json:"p,omitempty"`
	B []byte `json:"b,omitempty"`
	// rows
	Tb   string  `json:"tb,omitempty"`
	Rows [][]any `json:"rows,omitempty"`
	// end
	Counts map[string]int64 `json:"counts,omitempty"`
}

// backupSummary: Inhalt einer Sicherung (für Vorschau und Rückmeldung).
type backupSummary struct {
	At       int64            `json:"at"`
	App      string           `json:"app"`
	Store    string           `json:"store"`
	Orgs     int              `json:"orgs"`
	Users    int              `json:"users"`
	Hosts    int              `json:"hosts"`
	Sites    int              `json:"sites"`
	Machines int              `json:"machines"`
	Events   int64            `json:"events"`
	Counts   map[string]int64 `json:"counts"`
	Size     int64            `json:"size"`
	Enc      bool             `json:"encrypted"`
	Note     string           `json:"note,omitempty"`
}

func backupKey() string { return os.Getenv("BACKUP_KEY") }

func backupFileName(at time.Time) string {
	n := "pingwacht-backup-" + at.Format("2006-01-02-1504") + ".json.gz"
	if backupKey() != "" {
		n += ".enc"
	}
	return n
}

// ---------- Verschlüsselung (AES-256-GCM, blockweise) ----------

func deriveBackupKey(pass string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, backupKDFIter, 32)
}

type encWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	n      uint32
	buf    []byte
	closed bool
}

func newEncWriter(w io.Writer, pass string) (*encWriter, error) {
	salt := make([]byte, 16)
	prefix := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	key, err := deriveBackupKey(pass, salt)
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(blk)
	if _, err := w.Write(append(append([]byte(backupMagic), salt...), prefix...)); err != nil {
		return nil, err
	}
	return &encWriter{w: w, aead: aead, prefix: prefix, buf: make([]byte, 0, backupChunk)}, nil
}

func (e *encWriter) nonce() []byte {
	nc := make([]byte, 12)
	copy(nc, e.prefix)
	binary.BigEndian.PutUint32(nc[8:], e.n)
	e.n++
	return nc
}

func (e *encWriter) flushChunk(final bool) error {
	ad := []byte{0}
	if final {
		ad[0] = 1
	}
	ct := e.aead.Seal(nil, e.nonce(), e.buf, ad)
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr, uint32(len(ct)))
	hdr[4] = ad[0]
	if _, err := e.w.Write(hdr); err != nil {
		return err
	}
	_, err := e.w.Write(ct)
	e.buf = e.buf[:0]
	return err
}

func (e *encWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(backupChunk-len(e.buf), len(p))
		e.buf = append(e.buf, p[:k]...)
		p, n = p[k:], n+k
		if len(e.buf) == backupChunk {
			if err := e.flushChunk(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *encWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	return e.flushChunk(true)
}

type decReader struct {
	r      io.Reader
	aead   cipher.AEAD
	prefix []byte
	n      uint32
	buf    []byte
	done   bool
}

var errBackupKey = errors.New("Sicherung ist verschlüsselt, aber der Schlüssel (BACKUP_KEY) fehlt oder ist falsch")

func newDecReader(r io.Reader, pass string) (*decReader, error) {
	hdr := make([]byte, 24)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, errors.New("Sicherung unvollständig")
	}
	if pass == "" {
		return nil, errBackupKey
	}
	key, err := deriveBackupKey(pass, hdr[:16])
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(blk)
	return &decReader{r: r, aead: aead, prefix: hdr[16:24]}, nil
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.done {
			return 0, io.EOF
		}
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(d.r, hdr); err != nil {
			return 0, errors.New("Sicherung unvollständig (verschlüsselter Block fehlt)")
		}
		l := binary.BigEndian.Uint32(hdr)
		if l > backupChunk+64 {
			return 0, errors.New("Sicherung beschädigt")
		}
		ct := make([]byte, l)
		if _, err := io.ReadFull(d.r, ct); err != nil {
			return 0, errors.New("Sicherung unvollständig")
		}
		nc := make([]byte, 12)
		copy(nc, d.prefix)
		binary.BigEndian.PutUint32(nc[8:], d.n)
		d.n++
		pt, err := d.aead.Open(nil, nc, ct, hdr[4:5])
		if err != nil {
			if d.n == 1 {
				return 0, errBackupKey
			}
			return 0, errors.New("Sicherung beschädigt (Prüfsumme)")
		}
		d.buf = pt
		if hdr[4] == 1 {
			d.done = true
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

// ---------- Export ----------

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// writeBackup schreibt die komplette Sicherung nach w.
func (m *Monitor) writeBackup(w io.Writer) (map[string]int64, error) {
	var enc io.WriteCloser = nopWriteCloser{w}
	if k := backupKey(); k != "" {
		e, err := newEncWriter(w, k)
		if err != nil {
			return nil, err
		}
		enc = e
	}
	zw, _ := gzip.NewWriterLevel(enc, gzip.BestSpeed)
	bw := bufio.NewWriterSize(zw, 256<<10)
	je := json.NewEncoder(bw)
	counts := map[string]int64{}
	put := func(r backupRec) error {
		counts[r.T]++
		if r.T == "rows" {
			counts["row:"+r.Tb] += int64(len(r.Rows))
		}
		return je.Encode(r)
	}
	m.store.Flush()
	if err := put(backupRec{T: "meta", Format: backupFormat, V: backupVersion, App: version, At: time.Now().UnixMilli(), Store: m.store.Kind()}); err != nil {
		return nil, err
	}
	m.mu.RLock()
	cfg, err := json.Marshal(m.cfg)
	var machines []string
	for _, mc := range m.cfg.Machines {
		machines = append(machines, mc.ID)
	}
	m.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if err := put(backupRec{T: "config", Data: cfg}); err != nil {
		return nil, err
	}
	for _, e := range m.store.LoadEvents(0) {
		b, _ := json.Marshal(e)
		if err := put(backupRec{T: "event", Data: b}); err != nil {
			return nil, err
		}
	}
	switch st := m.store.(type) {
	case *PgStore:
		if err := st.backupRows(put); err != nil {
			return nil, err
		}
	case *FileStore:
		if bs, ok := m.store.(blobStore); ok {
			for _, id := range machines {
				if b, err := bs.LoadBlob(invKey(id)); err == nil && len(b) > 0 {
					if err := put(backupRec{T: "blob", K: invKey(id), B: b}); err != nil {
						return nil, err
					}
				}
			}
		}
		if err := st.backupFiles(put); err != nil {
			return nil, err
		}
	}
	end := map[string]int64{}
	for k, v := range counts {
		end[k] = v
	}
	if err := je.Encode(backupRec{T: "end", Counts: end}); err != nil {
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return end, enc.Close()
}

// skipBackupPath: Dateien, die nicht in die Sicherung gehören (eigene Formate oder Hilfsdateien).
func skipBackupPath(rel string) bool {
	top := strings.SplitN(rel, "/", 2)[0]
	switch {
	case rel == "config.json", strings.HasPrefix(rel, "events.jsonl"), top == "blobs", top == "backups",
		strings.HasPrefix(top, ".restore"), strings.HasSuffix(rel, ".tmp"):
		return true
	}
	return false
}

func (s *FileStore) backupFiles(put func(backupRec) error) error {
	s.mu.Lock()
	for _, df := range s.writers {
		df.w.Flush()
	}
	s.mu.Unlock()
	return filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(s.dir, p)
		rel = filepath.ToSlash(rel)
		if skipBackupPath(rel) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		return put(backupRec{T: "file", P: rel, B: b})
	})
}

var pgBackupTables = []struct {
	name string
	cols []string
}{
	{"pw_samples", []string{"host", "t", "r", "f"}},
	{"pw_minutes", []string{"host", "t", "n", "lost", "spk", "sum", "mn", "mx"}},
	{"pw_if", []string{"host", "t", "data"}},
	{"pw_ifinfo", []string{"host", "data"}},
	{"pw_blob", []string{"key", "data"}},
}

func pgTableCols(name string) []string {
	for _, t := range pgBackupTables {
		if t.name == name {
			return t.cols
		}
	}
	return nil
}

func (s *PgStore) backupRows(put func(backupRec) error) error {
	for _, tb := range pgBackupTables {
		rows, err := s.db.Query(`SELECT ` + strings.Join(tb.cols, ", ") + ` FROM ` + tb.name)
		if err != nil {
			return fmt.Errorf("%s lesen: %v", tb.name, err)
		}
		batch := make([][]any, 0, 2000)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			if tb.name == "pw_blob" {
				for _, r := range batch {
					k, _ := r[0].(string)
					d, _ := r[1].(string)
					if err := put(backupRec{T: "blob", K: k, B: []byte(d)}); err != nil {
						return err
					}
				}
			} else if err := put(backupRec{T: "rows", Tb: tb.name, Rows: batch}); err != nil {
				return err
			}
			batch = batch[:0]
			return nil
		}
		for rows.Next() {
			vals := make([]any, len(tb.cols))
			ptrs := make([]any, len(tb.cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			batch = append(batch, vals)
			if len(batch) == cap(batch) {
				if err := flush(); err != nil {
					rows.Close()
					return err
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
	}
	return nil
}

// makeBackupFile schreibt eine Sicherung in eine temporäre Datei und liefert Pfad, Größe und SHA-256.
func (m *Monitor) makeBackupFile() (string, int64, string, error) {
	f, err := os.CreateTemp("", "pingwacht-backup-*.tmp")
	if err != nil {
		return "", 0, "", err
	}
	h := sha256.New()
	_, err = m.writeBackup(io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, "", err
	}
	fi, _ := os.Stat(f.Name())
	return f.Name(), fi.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

func serveBackupFile(w http.ResponseWriter, r *http.Request, m *Monitor) {
	p, size, sum, err := m.makeBackupFile()
	if err != nil {
		log.Printf("Sicherung: %v", err)
		http.Error(w, "Sicherung fehlgeschlagen: "+err.Error(), 500)
		return
	}
	defer os.Remove(p)
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+backupFileName(time.Now())+`"`)
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("X-Sha256", sum)
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, f)
}

// ---------- Lesen / Prüfen ----------

// scanBackup liest eine Sicherung vollständig und ruft für jeden Datensatz fn auf.
// Fehlt der Abschluss oder passen die Anzahlen nicht, gibt es einen Fehler.
func scanBackup(r io.Reader, fn func(backupRec) error) (*backupSummary, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(len(backupMagic))
	sum := &backupSummary{Counts: map[string]int64{}}
	var src io.Reader = br
	if string(head) == backupMagic {
		br.Discard(len(backupMagic))
		d, err := newDecReader(br, backupKey())
		if err != nil {
			return nil, err
		}
		src, sum.Enc = d, true
	} else if len(head) < 2 || head[0] != 0x1f || head[1] != 0x8b {
		return nil, errors.New("keine PingWacht-Sicherung (unbekanntes Format)")
	}
	zr, err := gzip.NewReader(src)
	if err != nil {
		if errors.Is(err, errBackupKey) {
			return nil, err
		}
		return nil, errors.New("Sicherung beschädigt (gzip)")
	}
	dec := json.NewDecoder(zr)
	dec.UseNumber() // große Zahlen (Zeitstempel) exakt übernehmen
	first, ended := true, false
	for {
		var rec backupRec
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			if errors.Is(err, errBackupKey) {
				return nil, err
			}
			return nil, errors.New("Sicherung beschädigt oder unvollständig: " + err.Error())
		}
		if first {
			if rec.T != "meta" || rec.Format != backupFormat {
				return nil, errors.New("keine PingWacht-Sicherung")
			}
			if rec.V < 1 || rec.V > backupVersion {
				return nil, fmt.Errorf("Sicherungsformat %d wird nicht unterstützt", rec.V)
			}
			sum.At, sum.App, sum.Store = rec.At, rec.App, rec.Store
			first = false
			sum.Counts["meta"]++
			continue
		}
		if ended {
			return nil, errors.New("Sicherung beschädigt (Daten nach dem Abschluss)")
		}
		if rec.T == "end" {
			ended = true
			for k, v := range rec.Counts {
				if sum.Counts[k] != v {
					return nil, fmt.Errorf("Sicherung unvollständig (%s: %d statt %d)", k, sum.Counts[k], v)
				}
			}
			continue
		}
		sum.Counts[rec.T]++
		switch rec.T {
		case "config":
			var c Config
			if err := json.Unmarshal(rec.Data, &c); err != nil {
				return nil, errors.New("Konfiguration in der Sicherung fehlerhaft")
			}
			sum.Orgs, sum.Users, sum.Hosts, sum.Sites, sum.Machines = len(c.Orgs), len(c.Users), len(c.Hosts), len(c.Sites), len(c.Machines)
		case "event":
			sum.Events++
		case "rows":
			sum.Counts["row:"+rec.Tb] += int64(len(rec.Rows))
			if pgTableCols(rec.Tb) == nil {
				return nil, errors.New("Sicherung enthält unbekannte Tabelle " + rec.Tb)
			}
		case "file":
			if !safeRelPath(rec.P) {
				return nil, errors.New("Sicherung enthält ungültigen Dateipfad")
			}
		case "blob":
		default:
			return nil, errors.New("Sicherung enthält unbekannten Datensatz " + rec.T)
		}
		if fn != nil {
			if err := fn(rec); err != nil {
				return nil, err
			}
		}
	}
	if first {
		return nil, errors.New("Sicherung ist leer")
	}
	if !ended {
		return nil, errors.New("Sicherung unvollständig (Abschluss fehlt)")
	}
	if sum.Counts["config"] != 1 {
		return nil, errors.New("Sicherung enthält keine Konfiguration")
	}
	return sum, nil
}

func safeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "\x00") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return !skipBackupPath(p)
}

// ---------- Wiederherstellen ----------

// restoreBackup prüft die Datei vollständig und spielt sie dann ein. Danach ist der Speicher
// eingefroren; der Prozess muss neu starten.
func (m *Monitor) restoreBackup(path string) (*backupSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	sum, err := scanBackup(f, nil)
	f.Close()
	if err != nil {
		return nil, err
	}
	if !storeFrozen.CompareAndSwap(false, true) {
		return nil, errors.New("Eine Wiederherstellung läuft bereits")
	}
	switch st := m.store.(type) {
	case *PgStore:
		err = st.restore(path, sum)
	case *FileStore:
		err = st.restore(path, sum)
	default:
		err = errors.New("Speicherart unterstützt keine Wiederherstellung")
	}
	if err != nil {
		storeFrozen.Store(false)
		return nil, err
	}
	if sum.Store != m.store.Kind() {
		sum.Note = "Die Sicherung stammt aus einem anderen Speicher (" + sum.Store + ") – Konfiguration, Ereignisse und Inventar wurden übernommen, der Messwert-Verlauf nicht."
	}
	return sum, nil
}

func (s *FileStore) restore(path string, _ *backupSummary) error {
	s.Close()
	stage := filepath.Join(s.dir, ".restore-new")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	ev, err := os.Create(filepath.Join(stage, "events.jsonl"))
	if err != nil {
		return err
	}
	evw := bufio.NewWriter(ev)
	f, err := os.Open(path)
	if err != nil {
		ev.Close()
		return err
	}
	_, err = scanBackup(f, func(r backupRec) error {
		switch r.T {
		case "config":
			return os.WriteFile(filepath.Join(stage, "config.json"), r.Data, 0o600)
		case "event":
			evw.Write(r.Data)
			return evw.WriteByte('\n')
		case "blob":
			p := blobPath(stage, r.K)
			os.MkdirAll(filepath.Dir(p), 0o755)
			return os.WriteFile(p, r.B, 0o600)
		case "file":
			p := filepath.Join(stage, filepath.FromSlash(r.P))
			os.MkdirAll(filepath.Dir(p), 0o755)
			return os.WriteFile(p, r.B, 0o644)
		}
		return nil // Postgres-Zeilen lassen sich nicht in Dateien übernehmen
	})
	f.Close()
	if ferr := evw.Flush(); err == nil {
		err = ferr
	}
	ev.Close()
	if err != nil {
		os.RemoveAll(stage)
		return err
	}
	// Austausch: bisherigen Stand beiseitelegen, neuen Stand einsetzen
	old := filepath.Join(s.dir, ".restore-old-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(old, 0o755); err != nil {
		return err
	}
	ents, _ := os.ReadDir(s.dir)
	var moved []string
	undo := func() {
		for _, n := range moved {
			os.Rename(filepath.Join(old, n), filepath.Join(s.dir, n))
		}
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".restore") || n == "backups" {
			continue
		}
		if err := os.Rename(filepath.Join(s.dir, n), filepath.Join(old, n)); err != nil {
			undo()
			return err
		}
		moved = append(moved, n)
	}
	newEnts, _ := os.ReadDir(stage)
	for _, e := range newEnts {
		if err := os.Rename(filepath.Join(stage, e.Name()), filepath.Join(s.dir, e.Name())); err != nil {
			// zurückrollen
			for _, e2 := range newEnts {
				os.RemoveAll(filepath.Join(s.dir, e2.Name()))
			}
			undo()
			return err
		}
	}
	os.RemoveAll(stage)
	s.mu.Lock()
	s.aggCache = map[string][]Agg{}
	s.mu.Unlock()
	return nil
}

func (s *PgStore) restore(path string, _ *backupSummary) error {
	s.mu.Lock()
	s.raw, s.minutes = nil, map[minKey]*Agg{}
	s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`TRUNCATE pw_config, pw_samples, pw_minutes, pw_events, pw_if, pw_ifinfo, pw_blob`); err != nil {
		return err
	}
	evStmt, err := tx.Prepare(`INSERT INTO pw_events (id, start_t, end_t, data) VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer evStmt.Close()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = scanBackup(f, func(r backupRec) error {
		switch r.T {
		case "config":
			_, err := tx.Exec(`INSERT INTO pw_config (id, data) VALUES (1, $1)`, string(r.Data))
			return err
		case "event":
			var e Event
			if err := json.Unmarshal(r.Data, &e); err != nil {
				return errors.New("Ereignis fehlerhaft")
			}
			_, err := evStmt.Exec(e.ID, e.Start, e.End, string(r.Data))
			return err
		case "blob":
			_, err := tx.Exec(`INSERT INTO pw_blob (key, data) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data`, r.K, string(r.B))
			return err
		case "rows":
			cols := pgTableCols(r.Tb)
			stmt, err := tx.Prepare(pq.CopyIn(r.Tb, cols...))
			if err != nil {
				return err
			}
			for _, row := range r.Rows {
				if len(row) != len(cols) {
					stmt.Close()
					return errors.New("Zeile mit falscher Spaltenzahl in " + r.Tb)
				}
				if _, err := stmt.Exec(row...); err != nil {
					stmt.Close()
					return err
				}
			}
			if _, err := stmt.Exec(); err != nil {
				stmt.Close()
				return err
			}
			return stmt.Close()
		}
		return nil // Dateien aus dem Datei-Speicher (Messwerte) werden nicht übernommen
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cacheMu.Lock()
	s.aggCache = map[string][]Agg{}
	s.cacheMu.Unlock()
	return nil
}

// ---------- Sicherungsziele (Agenten) und Status ----------

type BackupCfg struct {
	On      bool  `json:"on"`
	Keep    int   `json:"keep,omitempty"`
	Enabled int64 `json:"enabled,omitempty"` // seit wann aktiv (für die Ausbleiben-Meldung)
}

type BackupStatus struct {
	At      int64  `json:"at"`             // letzte Rückmeldung
	OK      bool   `json:"ok"`             // letzte Sicherung erfolgreich
	LastOK  int64  `json:"lastOk"`         // Zeitpunkt der letzten erfolgreichen Sicherung
	Size    int64  `json:"size,omitempty"` // Größe der letzten erfolgreichen Sicherung
	File    string `json:"file,omitempty"`
	Count   int    `json:"count,omitempty"` // Anzahl Sicherungen am Ziel
	Err     string `json:"err,omitempty"`
	Alerted bool   `json:"alerted,omitempty"`
}

const backupAlertAfter = 36 * time.Hour

func (b *BackupCfg) keep() int {
	if b == nil || b.Keep <= 0 {
		return 14
	}
	return min(b.Keep, 365)
}

// backupTarget liefert die Sicherungs-Einstellung eines Standorts (nur Betreiber-Standorte).
func (m *Monitor) backupTarget(site SiteCfg) *BackupCfg {
	if site.Org != defaultOrgID && site.Org != "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if b := m.cfg.BackupTargets[site.ID]; b != nil && b.On {
		cp := *b
		return &cp
	}
	return nil
}

// checkBackups meldet einmalig, wenn ein Sicherungsziel seit über 36 h keine erfolgreiche Sicherung hat.
func (m *Monitor) checkBackups(now time.Time) {
	type alert struct{ site, name string }
	var send, recovered []alert
	m.mu.Lock()
	for _, sc := range m.cfg.Sites {
		b := m.cfg.BackupTargets[sc.ID]
		if b == nil || !b.On {
			continue
		}
		st := m.cfg.BackupStatus[sc.ID]
		last := b.Enabled
		if st != nil && st.LastOK > last {
			last = st.LastOK
		}
		overdue := last > 0 && now.Sub(time.UnixMilli(last)) > backupAlertAfter
		if st == nil {
			st = &BackupStatus{}
			if m.cfg.BackupStatus == nil {
				m.cfg.BackupStatus = map[string]*BackupStatus{}
			}
			m.cfg.BackupStatus[sc.ID] = st
		}
		switch {
		case overdue && !st.Alerted:
			st.Alerted = true
			send = append(send, alert{sc.ID, sc.Name})
		case !overdue && st.Alerted:
			st.Alerted = false
			recovered = append(recovered, alert{sc.ID, sc.Name})
		}
	}
	if len(send)+len(recovered) > 0 {
		m.saveConfigLocked()
	}
	m.mu.Unlock()
	for _, a := range send {
		title := "⚠️ Sicherung ausgeblieben: " + a.name
		body := "Seit über 36 Stunden gibt es keine erfolgreiche Datenbank-Sicherung auf „" + a.name + "“. Agent und Speicherplatz prüfen."
		m.notify(notice{kind: "backup", title: title, body: body, org: defaultOrgID})
		backupOpsMail(title, body)
	}
	for _, a := range recovered {
		m.notify(notice{kind: "backup", title: "✅ Sicherung wieder erfolgreich: " + a.name, body: "Die Datenbank-Sicherung auf „" + a.name + "“ läuft wieder.", org: defaultOrgID})
	}
}

var backupOpsMail = func(title, body string) {
	to := strings.TrimSpace(envOr("OPS_EMAIL", os.Getenv("PROVIDER_EMAIL")))
	if to == "" || !smtpConfigured() {
		return
	}
	if err := sendMail(splitAddrs(to), "[PingWacht] "+title, body, "", nil); err != nil {
		log.Printf("Hinweis-Mail Sicherung: %v", err)
	}
}

func (m *Monitor) setBackupStatus(siteID string, st BackupStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.BackupStatus == nil {
		m.cfg.BackupStatus = map[string]*BackupStatus{}
	}
	prev := m.cfg.BackupStatus[siteID]
	if prev != nil {
		st.Alerted = prev.Alerted
		if !st.OK {
			st.LastOK, st.Size, st.File = prev.LastOK, prev.Size, prev.File
			if st.Count == 0 {
				st.Count = prev.Count
			}
		}
	}
	m.cfg.BackupStatus[siteID] = &st
	m.saveConfigLocked()
}

// ---------- HTTP ----------

// GET /api/admin/backup → Sicherung herunterladen
func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET", 405)
		return
	}
	if storeFrozen.Load() {
		http.Error(w, "Wiederherstellung läuft – PingWacht startet gleich neu", 503)
		return
	}
	serveBackupFile(w, r, s.m)
}

// POST /api/admin/restore?dryRun=1 (Datei im Body oder als multipart „file“), confirm=WIEDERHERSTELLEN
func (s *Server) handleAdminRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", 405)
		return
	}
	if storeFrozen.Load() {
		http.Error(w, "Wiederherstellung läuft bereits", 409)
		return
	}
	dry := r.URL.Query().Get("dryRun") == "1"
	confirm := r.URL.Query().Get("confirm")
	var src io.Reader = http.MaxBytesReader(w, r.Body, 4<<30)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "Upload ungültig", 400)
			return
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				http.Error(w, "Datei fehlt", 400)
				return
			}
			if part.FormName() == "confirm" {
				b, _ := io.ReadAll(io.LimitReader(part, 100))
				confirm = strings.TrimSpace(string(b))
				continue
			}
			if part.FormName() == "file" {
				src = part
				break
			}
		}
	}
	if !dry && confirm != "WIEDERHERSTELLEN" {
		http.Error(w, "Bitte zur Bestätigung WIEDERHERSTELLEN eingeben", 400)
		return
	}
	tmp, err := os.CreateTemp("", "pingwacht-restore-*.tmp")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, src)
	tmp.Close()
	if err != nil {
		http.Error(w, "Upload abgebrochen: "+err.Error(), 400)
		return
	}
	if n == 0 {
		http.Error(w, "Datei ist leer", 400)
		return
	}
	if dry {
		f, _ := os.Open(tmp.Name())
		sum, err := scanBackup(f, nil)
		f.Close()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sum.Size = n
		if sum.Store != s.m.store.Kind() {
			sum.Note = "Die Sicherung stammt aus einem anderen Speicher (" + sum.Store + ") – übernommen werden Konfiguration, Ereignisse und Inventar, nicht der Messwert-Verlauf."
		}
		writeJSON(w, map[string]any{"ok": true, "dryRun": true, "summary": sum})
		return
	}
	sum, err := s.m.restoreBackup(tmp.Name())
	if err != nil {
		http.Error(w, "Wiederherstellung fehlgeschlagen – nichts geändert: "+err.Error(), 400)
		return
	}
	sum.Size = n
	who := "Betreiber"
	if p := principal(r); p != nil && p.User != nil {
		who = p.User.Email
	}
	log.Printf("Wiederherstellung durch %s: Sicherung vom %s (%d Kunden, %d Hosts, %d Ereignisse)", who,
		time.UnixMilli(sum.At).Format("02.01.2006 15:04"), sum.Orgs, sum.Hosts, sum.Events)
	writeJSON(w, map[string]any{"ok": true, "summary": sum, "restart": true})
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		restartFn()
	}()
}

type backupTargetView struct {
	Site    string        `json:"site"`
	Name    string        `json:"name"`
	On      bool          `json:"on"`
	Keep    int           `json:"keep"`
	Online  bool          `json:"online"`
	Version string        `json:"version,omitempty"`
	Status  *BackupStatus `json:"status,omitempty"`
}

// GET /api/admin/backup/status · POST /api/admin/backup/target {site,on,keep}
func (s *Server) handleBackupTargets(w http.ResponseWriter, r *http.Request) {
	m := s.m
	if r.Method == http.MethodPost {
		var req struct {
			Site string `json:"site"`
			On   bool   `json:"on"`
			Keep int    `json:"keep"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		m.mu.Lock()
		var site *SiteCfg
		for i := range m.cfg.Sites {
			if m.cfg.Sites[i].ID == req.Site {
				site = &m.cfg.Sites[i]
			}
		}
		if site == nil || (site.Org != defaultOrgID && site.Org != "") || site.Kind == siteKindUniFi {
			m.mu.Unlock()
			http.Error(w, "Sicherungen nur auf Standorten mit Agent des Betreibers", 400)
			return
		}
		if m.cfg.BackupTargets == nil {
			m.cfg.BackupTargets = map[string]*BackupCfg{}
		}
		b := m.cfg.BackupTargets[req.Site]
		if b == nil {
			b = &BackupCfg{}
			m.cfg.BackupTargets[req.Site] = b
		}
		if req.On && !b.On {
			b.Enabled = time.Now().UnixMilli()
		}
		b.On = req.On
		b.Keep = max(1, min(req.Keep, 365))
		if req.Keep == 0 {
			b.Keep = 14
		}
		m.saveConfigLocked()
		m.mu.Unlock()
	}
	m.mu.RLock()
	out := map[string]any{"encrypted": backupKey() != "", "store": m.store.Kind()}
	list := []backupTargetView{}
	for _, sc := range m.cfg.Sites {
		if (sc.Org != defaultOrgID && sc.Org != "") || sc.Kind == siteKindUniFi {
			continue
		}
		v := backupTargetView{Site: sc.ID, Name: sc.Name, Keep: 14}
		if b := m.cfg.BackupTargets[sc.ID]; b != nil {
			v.On, v.Keep = b.On, b.keep()
		}
		if st := m.cfg.BackupStatus[sc.ID]; st != nil {
			cp := *st
			v.Status = &cp
		}
		if st := m.sites[sc.ID]; st != nil {
			st.mu.Lock()
			v.Online = st.lastSeen > 0 && time.Since(time.UnixMilli(st.lastSeen)) < 2*time.Minute
			v.Version = st.version
			st.mu.Unlock()
		}
		list = append(list, v)
	}
	m.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	out["targets"] = list
	writeJSON(w, out)
}

// GET /api/agent/backup → Sicherung für den Agenten (nur wenn der Standort Sicherungsziel ist)
// POST /api/agent/backup {ok, size, file, count, err} → Ergebnis melden
func (s *Server) handleAgentBackup(w http.ResponseWriter, r *http.Request) {
	site, ok := s.m.siteByToken(bearer(r))
	if !ok {
		http.Error(w, "Token ungültig", 401)
		return
	}
	if s.m.backupTarget(site) == nil {
		http.Error(w, "Für diesen Standort sind keine Sicherungen eingerichtet", 403)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			OK    bool   `json:"ok"`
			Size  int64  `json:"size"`
			File  string `json:"file"`
			Count int    `json:"count"`
			Err   string `json:"err"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		now := time.Now().UnixMilli()
		st := BackupStatus{At: now, OK: req.OK, Size: req.Size, File: clip(req.File, 120), Count: req.Count, Err: clip(req.Err, 300)}
		if req.OK {
			st.LastOK = now
			st.Err = ""
		}
		s.m.setBackupStatus(site.ID, st)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if storeFrozen.Load() {
		http.Error(w, "Wiederherstellung läuft", 503)
		return
	}
	serveBackupFile(w, r, s.m)
}

// ---------- Agent: Sicherungen ablegen ----------

const backupPrefix = "pingwacht-backup-"

func (a *Agent) backupDir() string {
	if d := strings.TrimSpace(a.file.BackupDir); d != "" {
		return d
	}
	return filepath.Join(a.dataDir, "backups")
}

// listBackups liefert die vorhandenen Sicherungen (älteste zuerst).
func listBackups(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), backupPrefix) && !strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// rotateBackups löscht die ältesten Sicherungen über keep.
func rotateBackups(dir string, keep int) int {
	files := listBackups(dir)
	for len(files) > keep && keep > 0 {
		os.Remove(filepath.Join(dir, files[0]))
		files = files[1:]
	}
	return len(files)
}

func lastBackupTime(dir string) time.Time {
	files := listBackups(dir)
	if len(files) == 0 {
		return time.Time{}
	}
	fi, err := os.Stat(filepath.Join(dir, files[len(files)-1]))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// backupDue: täglich zwischen 03:00 und 04:00 – oder sofort, wenn die letzte Sicherung älter als 24 h ist.
func backupDue(now, last time.Time) bool {
	if last.IsZero() || now.Sub(last) > 24*time.Hour {
		return true
	}
	if now.Hour() == 3 {
		y, m, d := now.Date()
		ly, lm, ld := last.Date()
		return y != ly || m != lm || d != ld || last.Hour() < 3
	}
	return false
}

var backupHTTP = &http.Client{Timeout: 60 * time.Minute}

// runBackup lädt eine Sicherung vom Server, prüft sie und legt sie ab.
func (a *Agent) runBackup(keep int) (string, int64, int, error) {
	dir := a.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, 0, fmt.Errorf("Ordner %s: %v", dir, err)
	}
	r, err := http.NewRequest("GET", a.file.Server+"/api/agent/backup", nil)
	if err != nil {
		return "", 0, 0, err
	}
	r.Header.Set("Authorization", "Bearer "+a.file.Token)
	r.Header.Set("X-Agent-Version", version)
	resp, err := backupHTTP.Do(r)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", 0, 0, fmt.Errorf("Server: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	name := backupPrefix + time.Now().Format("2006-01-02-1504") + ".json.gz"
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(cd, "filename=\"") {
		n := cd[strings.Index(cd, "filename=\"")+10:]
		if i := strings.Index(n, "\""); i > 0 {
			n = filepath.Base(n[:i])
			if strings.HasPrefix(n, backupPrefix) {
				name = n
			}
		}
	}
	tmp := filepath.Join(dir, name+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, 0, fmt.Errorf("Download abgebrochen: %v", err)
	}
	if want := resp.Header.Get("X-Sha256"); want != "" && !strings.EqualFold(want, hex.EncodeToString(h.Sum(nil))) {
		os.Remove(tmp)
		return "", 0, 0, errors.New("Prüfsumme stimmt nicht – Sicherung verworfen")
	}
	if n < 20 {
		os.Remove(tmp)
		return "", 0, 0, errors.New("Sicherung ist leer")
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return "", 0, 0, err
	}
	count := rotateBackups(dir, keep)
	return name, n, count, nil
}

func (a *Agent) reportBackup(name string, size int64, count int, err error) {
	body := map[string]any{"ok": err == nil, "size": size, "file": name, "count": count}
	if err != nil {
		body["err"] = err.Error()
	}
	b, _ := json.Marshal(body)
	r, rerr := http.NewRequest("POST", a.file.Server+"/api/agent/backup", bytes.NewReader(b))
	if rerr != nil {
		return
	}
	r.Header.Set("Authorization", "Bearer "+a.file.Token)
	r.Header.Set("Content-Type", "application/json")
	if resp, err := a.client.Do(r); err == nil {
		resp.Body.Close()
	}
}

// backupLoop prüft alle 5 Minuten, ob eine Sicherung fällig ist.
func (a *Agent) backupLoop() {
	var lastTry time.Time
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	check := func() {
		a.mu.Lock()
		on, keep := a.backupOn, a.backupKeep
		a.mu.Unlock()
		if !on || a.sysOnly {
			return
		}
		now := time.Now()
		if !backupDue(now, lastBackupTime(a.backupDir())) || now.Sub(lastTry) < 55*time.Minute {
			return
		}
		lastTry = now
		name, size, count, err := a.runBackup(keep)
		if err != nil {
			log.Printf("Sicherung fehlgeschlagen: %v", err)
		} else {
			log.Printf("Sicherung gespeichert: %s (%d KB, %d vorhanden)", name, size/1024, count)
		}
		a.reportBackup(name, size, count, err)
	}
	time.Sleep(90 * time.Second) // erst Konfiguration laden lassen
	check()
	for range tick.C {
		check()
	}
}
