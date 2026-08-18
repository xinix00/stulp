'use strict';

// De service worker bestaat voor één ding: een pushbericht binnenlaten terwijl
// er geen tabblad open is. Hij cachet niets en onderschept geen verzoeken -- een
// offline kopie van Manage zou een huis tonen zoals het gisteren was, en dat is
// erger dan een pagina die zegt dat hij de hub niet kan bereiken.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));

self.addEventListener('push', event => {
  // Elk pushbericht moet iets tonen: browsers staan pushen alleen toe onder de
  // belofte dat het zichtbaar is, en een stille push kost het abonnement.
  let payload = {};
  if (event.data) {
    try { payload = event.data.json(); } catch (error) { payload = { body: event.data.text() }; }
  }
  // Geen tag, dus meldingen stapelen op in plaats van elkaar te vervangen. Twee
  // keer aanbellen is twee keer aanbellen, en een deurbel die een eerdere melding
  // opslokt kan net die ene zijn die je gemist hebt.
  const options = {
    body: payload.body || '',
    icon: '/assets/icon-192.png',
    badge: '/assets/icon-192.png',
    data: { url: payload.url || '/' },
    timestamp: Date.now(),
  };
  // De foto komt als adres mee en wordt hier opgehaald: er past een regel tekst
  // in één pushbericht, geen megabyte aan camerabeeld. Android toont hem groot;
  // Safari kent deze optie niet en laat alleen kop en tekst zien.
  if (payload.image) options.image = payload.image;
  event.waitUntil(self.registration.showNotification(payload.title || 'Stulp', options));
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const target = new URL(event.notification.data?.url || '/', self.location.origin).href;
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of windows) {
      if (client.url === target && 'focus' in client) return client.focus();
    }
    return self.clients.openWindow(target);
  })());
});
