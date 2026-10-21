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
