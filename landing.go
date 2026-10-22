package main

import (
	"io/fs"
	"net/http"
	"os"
	"strings"
)

// Produktseite für pingwacht.de – von derselben App ausgeliefert.
// Hosts aus LANDING_HOSTS (kommagetrennt, Standard „pingwacht.de,www.pingwacht.de“):
//   /             → web/landing.html
//   /landing/…    → Bilder und Assets der Produktseite
//   /robots.txt, /sitemap.xml
//   www.<domain>  → 301 auf <domain>
//   /login, /signup → zur App-Adresse (publicBase), damit die Sitzung dort entsteht
// Alles andere (Impressum, Datenschutz, Icons) läuft normal weiter.

func landingHosts() []string {
	v := os.Getenv("LANDING_HOSTS")
	if strings.TrimSpace(v) == "" {
		v = "pingwacht.de,www.pingwacht.de"
	}
	var out []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func requestHost(r *http.Request) string {
	h := r.Header.Get("X-Forwarded-Host")
	if h == "" {
		h = r.Host
	}
	h = strings.ToLower(strings.TrimSpace(strings.Split(h, ",")[0]))
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

func isLandingHost(h string) bool {
	for _, x := range landingHosts() {
		if h == x {
			return true
		}
	}
	return false
}

func landingMiddleware(next http.Handler) http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHost(r)
		if !isLandingHost(host) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(host, "www.") && isLandingHost(strings.TrimPrefix(host, "www.")) {
			http.Redirect(w, r, "https://"+strings.TrimPrefix(host, "www.")+r.URL.RequestURI(), http.StatusMovedPermanently)
			return
		}
		app := publicBase(r)
		if strings.Contains(app, "://"+host) && os.Getenv("PUBLIC_URL") == "" {
			app = defaultPublicURL // Produktseite verweist auf die App-Adresse
		}
		appIsHere := strings.Contains(app, "://"+host)
		switch p := r.URL.Path; {
		case p == "/" || p == "/index.html":
			b, err := webFS.ReadFile("web/landing.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			html := strings.ReplaceAll(string(b), "__APP__", app)
			html = strings.ReplaceAll(html, "__SITE__", "https://"+host)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.Write([]byte(html))
		case strings.HasPrefix(p, "/landing/"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
			static.ServeHTTP(w, r)
		case p == "/robots.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("User-agent: *\nAllow: /\nDisallow: /api/\nSitemap: https://" + host + "/sitemap.xml\n"))
		case p == "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
				`<url><loc>https://` + host + `/</loc><changefreq>weekly</changefreq><priority>1.0</priority></url>` +
				`<url><loc>https://` + host + `/impressum</loc></url><url><loc>https://` + host + `/datenschutz</loc></url></urlset>` + "\n"))
		case (p == "/login" || p == "/signup") && !appIsHere:
			http.Redirect(w, r, app+p, http.StatusFound)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
