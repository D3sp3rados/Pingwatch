package main

// Satellitenkarte: Geräte der Netzkarte per Adresse platzieren.
// Die Adresse steckt bei Kunden meist im Namen („Kuhnert-Laufenburgerstrasse11“, „Schmid-Im Schaechle4“),
// der Ort im Feld „Ort“. Gesucht wird über OpenStreetMap (Nominatim, höchstens 1 Anfrage pro Sekunde,
// Ergebnisse werden zwischengespeichert). Übernommen werden nur Treffer mit Hausnummer –
// alles andere (Sektoren, Masten, ungenaue Treffer) wird von Hand auf die Karte gezogen.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type geoAddr struct {
	Street string // „Laufenburger Straße 11“
	Town   string
}

var (
	geoIPRe      = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
	geoParenRe   = regexp.MustCompile(`\([^)]*\)?`)
	geoNoiseRe   = regexp.MustCompile(`(?i)\b(hausanschluss|zuleitung|gek(ü|ue|u)ndigt|guendigt|glas|stiegeler|stadtwerke|ssid\S*|umgezogen|weggezogen|abgeschaltet|abgebaut|neu|alt|basis|basisstation|ppoe|pppoe|usv|sektor|fb\d+|af\d+\w*|wave\S*|60ghz|5ghz|ap|cpe|router|mikrotik|ubnt|lumina|netonix|prism|airfiber\w*|nano\w*|rocket\w*)\b`)
	geoDateRe    = regexp.MustCompile(`\b\d{1,2}\.\d{1,2}\.\d{2,4}\b`)
	geoNumTail   = regexp.MustCompile(`^(.*?[A-Za-zÄÖÜäöüß\.])\s*(\d{1,4}\s?[a-zA-Z]?)$`)
	geoNumOnly   = regexp.MustCompile(`^\d{1,4}\s?[a-zA-Z]?$`)
	geoGenericRe = regexp.MustCompile(`(?i)^(weg|stra(ss|ß)e|str|gasse|platz|allee|ring|steig|pfad)$`)
	geoPrefixRe  = regexp.MustCompile(`(?i)^(im|am|an|auf|zum|zur|in|untere|obere|unterer|oberer|hinter|alte|neue|alter|neuer|kleine|grosse|große)$`)
)

// parseGeoAddr holt Straße + Hausnummer aus einem Gerätenamen.
func parseGeoAddr(name, zone string) (geoAddr, bool) {
	s := geoIPRe.ReplaceAllString(name, " ")
	s = geoParenRe.ReplaceAllString(s, " ")
	s = geoDateRe.ReplaceAllString(s, " ")
	s = geoNoiseRe.ReplaceAllString(s, " ")
	s = strings.NewReplacer("+1", " ", "+", " ", "/", " ", ",", " ", "_", " ", "->", " ").Replace(s)
	// Teile an Bindestrichen: meist „Name-Straße12“
	parts := strings.Split(s, "-")
	for i := len(parts) - 1; i >= 0; i-- {
		words := strings.Fields(parts[i])
		for j := len(words) - 1; j >= 0; j-- {
			w := words[j]
			var street, num string
			if m := geoNumTail.FindStringSubmatch(w); m != nil {
				street, num = m[1], m[2]
			} else if geoNumOnly.MatchString(w) && j > 0 {
				street, num = words[j-1], w
				j--
			} else {
				continue
			}
			// Vorwörter wie „Im“, „Untere“ und mehrteilige Straßen („Untere Hattelaecker“)
			k := j - 1
			for k >= 0 && (geoPrefixRe.MatchString(words[k]) || (k == j-1 && j >= 2 && geoPrefixRe.MatchString(words[k-1]))) {
				street = words[k] + " " + street
				k--
			}
			// nur „Weg“/„Straße“: Wort davor gehört dazu („Waldshuter Strasse2“, „Joseph-Tröndlin-Weg 14“)
			if geoGenericRe.MatchString(strings.Trim(street, ".- ")) {
				first := ""
				if k >= 0 {
					first = words[k]
					street = first + " " + street
				}
				// mit Bindestrich geschriebene Straßen: vorherige Teile anhängen (nicht bei „…er Straße“)
				for pi := i - 1; pi >= 0 && (first == "" || !strings.HasSuffix(strings.ToLower(first), "er")) && (k <= 0); pi-- {
					pw := strings.Fields(parts[pi])
					if len(pw) == 0 {
						break
					}
					first = pw[len(pw)-1]
					sep := "-"
					if k < 0 && pi == i-1 {
						sep = "-" // „Tröndlin-Weg“
					}
					street = first + sep + street
					if len(pw) > 1 {
						break
					}
					k = 0
				}
			}
			street = strings.Trim(street, ".- ")
			if len([]rune(street)) < 3 || strings.ContainsAny(street, "0123456789") {
				continue
			}
			street = strings.Replace(normStreet(street), " straße", " Straße", 1)
			return geoAddr{Street: street + " " + strings.ReplaceAll(num, " ", ""), Town: strings.TrimSpace(zone)}, true
		}
	}
	return geoAddr{}, false
}

func normStreet(s string) string {
	low := strings.ToLower(s)
	switch {
	case strings.HasSuffix(low, "strasse"):
		s = s[:len(s)-len("strasse")] + "straße"
	case strings.HasSuffix(low, "str."):
		s = s[:len(s)-len("str.")] + "straße"
	case strings.HasSuffix(low, "str"):
		s = s[:len(s)-len("str")] + "straße"
	}
	return s
}

// umlaute: „Ruettehof“ → „Rüttehof“ (zweiter Versuch)
func umlaute(s string) string {
	return strings.NewReplacer("ae", "ä", "oe", "ö", "ue", "ü", "Ae", "Ä", "Oe", "Ö", "Ue", "Ü").Replace(s)
}

// ---------- Nominatim ----------

type geoHit struct {
	Lat, Lon float64
	OK       bool // mit Hausnummer gefunden
}

var geoClient = &http.Client{Timeout: 20 * time.Second}

const geoViewbox = "7.93,47.92,8.45,47.54" // Landkreis Waldshut bis Löffingen/Bonndorf (Glas) – Kunden nur in Deutschland

func nominatim(q string) (geoHit, error) {
	u := "https://nominatim.openstreetmap.org/search?format=jsonv2&addressdetails=1&limit=1&countrycodes=de&bounded=1&viewbox=" + geoViewbox + "&q=" + url.QueryEscape(q)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "PingWacht/"+version+" (Netzkarte; https://app.pingwacht.de)")
	req.Header.Set("Accept-Language", "de")
	resp, err := geoClient.Do(req)
	if err != nil {
		return geoHit{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return geoHit{}, fmt.Errorf("Adresssuche: HTTP %d", resp.StatusCode)
	}
	var out []struct {
		Lat, Lon string
		Address  map[string]string `json:"address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return geoHit{}, err
	}
	if len(out) == 0 {
		return geoHit{}, nil
	}
	lat, _ := strconv.ParseFloat(out[0].Lat, 64)
	lon, _ := strconv.ParseFloat(out[0].Lon, 64)
	return geoHit{lat, lon, out[0].Address["house_number"] != ""}, nil
}

// ---------- Hintergrund-Suche je Karte ----------

type geoJob struct {
	Running  bool   `json:"running"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	Found    int    `json:"found"`
	NoAddr   int    `json:"noAddr"`
	NotFound int    `json:"notFound"`
	Err      string `json:"err,omitempty"`
	At       int64  `json:"at"`
}

var geoState = struct {
	sync.Mutex
	jobs  map[string]*geoJob
	cache map[string]geoHit
	last  time.Time
}{jobs: map[string]*geoJob{}, cache: map[string]geoHit{}}

// geoLookup mit Zwischenspeicher und höchstens einer Anfrage pro Sekunde.
func geoLookup(q string) (geoHit, error) {
	geoState.Lock()
	if h, ok := geoState.cache[q]; ok {
		geoState.Unlock()
		return h, nil
	}
	wait := time.Until(geoState.last.Add(1100 * time.Millisecond))
	geoState.last = time.Now().Add(max(wait, 0))
	geoState.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	h, err := nominatim(q)
	if err == nil {
		geoState.Lock()
		geoState.cache[q] = h
		geoState.Unlock()
	}
	return h, err
}

func geoQueries(a geoAddr) []string {
	qs := []string{}
	if a.Town != "" {
		qs = append(qs, a.Street+", "+a.Town)
		if u := umlaute(a.Street); u != a.Street {
			qs = append(qs, u+", "+a.Town)
		}
	}
	qs = append(qs, a.Street)
	if u := umlaute(a.Street); u != a.Street {
		qs = append(qs, u)
	}
	return qs
}

func geocodeAddr(a geoAddr) (geoHit, error) {
	for _, q := range geoQueries(a) {
		h, err := geoLookup(q)
		if err != nil {
			return h, err
		}
		if h.OK {
			return h, nil
		}
	}
	return geoHit{}, nil
}

func (m *Monitor) startGeocode(mapID string) *geoJob {
	geoState.Lock()
	if j := geoState.jobs[mapID]; j != nil && j.Running {
		cp := *j
		geoState.Unlock()
		return &cp
	}
	j := &geoJob{Running: true, At: time.Now().UnixMilli()}
	geoState.jobs[mapID] = j
	geoState.Unlock()
	go m.runGeocode(mapID, j)
	cp := *j
	return &cp
}

func (m *Monitor) runGeocode(mapID string, j *geoJob) {
	d, err := m.loadMap(mapID)
	set := func(f func()) { geoState.Lock(); f(); geoState.Unlock() }
	if err != nil {
		set(func() { j.Running, j.Err = false, err.Error() })
		return
	}
	type todo struct {
		id string
		a  geoAddr
	}
	var list []todo
	noAddr := 0
	for _, n := range d.Nodes {
		if n.Lat != 0 || n.Lon != 0 {
			continue
		}
		if a, ok := parseGeoAddr(n.Name, n.Zone); ok {
			list = append(list, todo{n.ID, a})
		} else {
			noAddr++
		}
	}
	set(func() { j.Total, j.NoAddr = len(list), noAddr })
	found := map[string]geoHit{}
	flush := func() {
		if len(found) == 0 {
			return
		}
		mapEditMu.Lock()
		defer mapEditMu.Unlock()
		d, err := m.loadMap(mapID)
		if err != nil {
			return
		}
		for i := range d.Nodes {
			if h, ok := found[d.Nodes[i].ID]; ok && d.Nodes[i].Lat == 0 && d.Nodes[i].Lon == 0 {
				d.Nodes[i].Lat, d.Nodes[i].Lon, d.Nodes[i].Geo = h.Lat, h.Lon, "auto"
			}
		}
		if m.saveMap(mapID, d) == nil {
			found = map[string]geoHit{}
		}
	}
	for i, t := range list {
		h, err := geocodeAddr(t.a)
		if err != nil {
			set(func() { j.Err = err.Error() })
			log.Printf("Netzkarte %s Adresssuche: %v", mapID, err)
			if strings.Contains(err.Error(), "HTTP 429") || strings.Contains(err.Error(), "HTTP 403") {
				break
			}
			continue
		}
		set(func() {
			j.Done = i + 1
			if h.OK {
				j.Found++
			} else {
				j.NotFound++
			}
		})
		if h.OK {
			found[t.id] = h
		}
		if len(found) >= 20 {
			flush()
		}
	}
	flush()
	set(func() { j.Running = false; j.At = time.Now().UnixMilli() })
}

// GET/POST /api/maps/geocode?id= – Adresssuche starten bzw. Stand abfragen
func (s *Server) handleMapGeocode(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.URL.Query().Get("id")
	s.m.mu.RLock()
	_, _, ok := s.m.mapByID(p.Org, id)
	s.m.mu.RUnlock()
	if !ok {
		http.Error(w, "Karte unbekannt", 404)
		return
	}
	if r.Method == http.MethodPost {
		if !p.canEdit() {
			http.Error(w, "keine Berechtigung", 403)
			return
		}
		writeJSON(w, s.m.startGeocode(id))
		return
	}
	// Plan für die Suche im Browser (falls der Server OpenStreetMap nicht erreicht)
	if r.URL.Query().Get("plan") == "1" {
		d, err := s.m.loadMap(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type item struct {
			ID string   `json:"id"`
			Qs []string `json:"qs"`
		}
		out := []item{}
		for _, n := range d.Nodes {
			if n.Lat != 0 || n.Lon != 0 {
				continue
			}
			if a, ok := parseGeoAddr(n.Name, n.Zone); ok {
				out = append(out, item{n.ID, geoQueries(a)})
			}
		}
		writeJSON(w, map[string]any{"items": out, "viewbox": geoViewbox})
		return
	}
	geoState.Lock()
	var out geoJob
	if j := geoState.jobs[id]; j != nil {
		out = *j
	}
	geoState.Unlock()
	writeJSON(w, out)
}
