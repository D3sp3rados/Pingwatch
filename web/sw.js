// PingWatch – Service Worker für Push-Benachrichtigungen
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(self.clients.claim()));

self.addEventListener('push', e => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch (_) { d = { title: 'PingWatch', body: e.data ? e.data.text() : '' }; }
  e.waitUntil(self.registration.showNotification(d.title || 'PingWatch', {
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

// Offline-Hinweis statt Browser-Fehlerseite, wenn die App ohne Netz geöffnet wird
self.addEventListener('fetch', e => {
  if (e.request.mode !== 'navigate') return;
  e.respondWith(fetch(e.request).catch(() => new Response(
    '<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>PingWatch – offline</title>' +
    '<body style="margin:0;min-height:100vh;display:grid;place-items:center;background:#0b1120;color:#e2e8f0;font:15px system-ui;padding:16px;text-align:center">' +
    '<div><div style="font-size:40px">📡</div><h2>Keine Verbindung</h2><p style="color:#8b9ab4">Dieses Gerät ist offline. PingWatch lädt, sobald wieder Netz da ist.</p>' +
    '<button onclick="location.reload()" style="padding:10px 18px;border:0;border-radius:10px;background:#34d399;font-weight:600">Neu laden</button></div>',
    { headers: { 'Content-Type': 'text/html; charset=utf-8' } })));
});
