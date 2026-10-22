// PingWacht – Service Worker für Push-Benachrichtigungen
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(self.clients.claim()));

self.addEventListener('push', e => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch (_) { d = { title: 'PingWacht', body: e.data ? e.data.text() : '' }; }
  e.waitUntil(self.registration.showNotification(d.title || 'PingWacht', {
    body: d.body || '',
    icon: '/icon-180.png',
    badge: '/icon-180.png',
    tag: (d.kind || '') + '|' + (d.host || ''),
    renotify: true,
    requireInteraction: !!d.urgent,
    data: { url: d.url || '/' }
  }));
});

self.addEventListener('notificationclick', e => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || '/';
  e.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const c of all) {
      if (new URL(c.url).origin === self.location.origin) { await c.focus(); c.navigate(url); return; }
    }
    await self.clients.openWindow(url);
  })());
});

// Eigene Seiten statt Browser-/Proxy-Fehlerseiten:
//  - offline (kein Netz)
//  - „wird gerade aktualisiert“ (502/503/504 vom Proxy, z. B. während eines Redeploys) – lädt automatisch neu
const PW_LOGO = 'data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNTYgMjU2Ij4KPGRlZnM+PGxpbmVhckdyYWRpZW50IGlkPSJzdyIgeDE9IjAiIHkxPSIxIiB4Mj0iMSIgeTI9IjAiPjxzdG9wIG9mZnNldD0iMCIgc3RvcC1jb2xvcj0iIzM0ZDM5OSIgc3RvcC1vcGFjaXR5PSIwIi8+PHN0b3Agb2Zmc2V0PSIxIiBzdG9wLWNvbG9yPSIjMzRkMzk5IiBzdG9wLW9wYWNpdHk9Ii41NSIvPjwvbGluZWFyR3JhZGllbnQ+PC9kZWZzPgo8cmVjdCB3aWR0aD0iMjU2IiBoZWlnaHQ9IjI1NiIgcng9IjU2IiBmaWxsPSIjMGUxNzJhIi8+CjxjaXJjbGUgY3g9IjEyOCIgY3k9IjEzNCIgcj0iOTIiIGZpbGw9Im5vbmUiIHN0cm9rZT0iIzI0NDY1YyIgc3Ryb2tlLXdpZHRoPSI3Ii8+CjxjaXJjbGUgY3g9IjEyOCIgY3k9IjEzNCIgcj0iNTYiIGZpbGw9Im5vbmUiIHN0cm9rZT0iIzI0NDY1YyIgc3Ryb2tlLXdpZHRoPSI3Ii8+CjxwYXRoIGQ9Ik0xMjggMTM0IEwxMjggNDIgQTkyIDkyIDAgMCAxIDIwNy43IDg4IFoiIGZpbGw9InVybCgjc3cpIi8+Cjxwb2x5bGluZSBwb2ludHM9IjMwLDEzNCA4NCwxMzQgOTksMTUyIDExNyw5MiAxMzcsMTkwIDE1MiwxMTggMTYzLDEzNCAyMjYsMTM0IiBmaWxsPSJub25lIiBzdHJva2U9IiMzNGQzOTkiIHN0cm9rZS13aWR0aD0iMTciIHN0cm9rZS1saW5lam9pbj0icm91bmQiIHN0cm9rZS1saW5lY2FwPSJyb3VuZCIvPgo8Y2lyY2xlIGN4PSIxOTAiIGN5PSI3MCIgcj0iMTQiIGZpbGw9IiMzNGQzOTkiLz4KPC9zdmc+Cg==';
function pwPage(icon, title, text, auto) {
  return new Response(
    '<!doctype html><html lang="de"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>PingWacht – ' + title + '</title>' +
    '<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b1120;color:#e2e8f0;font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px;text-align:center}' +
    '.c{max-width:420px}.logo{display:flex;align-items:center;justify-content:center;gap:10px;font-weight:700;font-size:20px;margin-bottom:22px}.logo img{width:36px;height:36px;border-radius:9px}' +
    '.i{font-size:42px}h2{margin:8px 0 6px}p{color:#8b9ab4;margin:0 0 18px}.b{padding:10px 18px;border:0;border-radius:10px;background:#34d399;color:#0b1120;font-weight:700;font-size:15px;cursor:pointer}' +
    '.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#34d399;margin:0 3px;animation:p 1.2s infinite}.dot:nth-child(2){animation-delay:.2s}.dot:nth-child(3){animation-delay:.4s}' +
    '@keyframes p{0%,80%,100%{opacity:.25}40%{opacity:1}}.s{font-size:12.5px;color:#64748b;margin-top:14px}</style>' +
    '<div class="c"><div class="logo"><img src="' + PW_LOGO + '" alt=""><span>Ping<span style="color:#34d399">Wacht</span></span></div>' +
    '<div class="i">' + icon + '</div><h2>' + title + '</h2><p>' + text + '</p>' +
    (auto ? '<div><span class="dot"></span><span class="dot"></span><span class="dot"></span></div><div class="s" id="s">Lädt automatisch neu, sobald PingWacht wieder da ist.</div>' : '') +
    '<p style="margin-top:18px"><button class="b" onclick="location.reload()">Neu laden</button></p></div>' +
    (auto ? '<script>(function t(){fetch("/healthz",{cache:"no-store"}).then(function(r){if(r.ok)location.reload();else setTimeout(t,4000)}).catch(function(){setTimeout(t,4000)})})()</script>' : ''),
    { status: 200, headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' } });
}

self.addEventListener('fetch', e => {
  if (e.request.mode !== 'navigate') return;
  e.respondWith(fetch(e.request).then(r => {
    if ((r.status === 502 || r.status === 503 || r.status === 504) && !r.headers.get('X-PingWacht')) {
      return pwPage('🔧', 'Einen Moment bitte', 'PingWacht wird gerade aktualisiert oder neu gestartet. Das dauert meist nur ein bis zwei Minuten.', true);
    }
    return r;
  }).catch(() => pwPage('📡', 'Keine Verbindung', 'Dieses Gerät ist offline oder PingWacht ist gerade nicht erreichbar.', true)));
});
