// dun's service worker: just enough to install, and to open the app shell
// when the network is gone. The app itself needs the service, so nothing
// under /api is ever cached, and the WebSocket is not touched.
//
// Pages: network first (a new build is picked up at once), the cached shell
// when offline. Hashed bundles (/_expo/…) and icons: cache first.
const CACHE = 'dun-v1';
// Type-checked through public/tsconfig.json (webworker lib).
const sw = /** @type {ServiceWorkerGlobalScope} */ (/** @type {unknown} */ (self));
const SHELL = ['/', '/manifest.json', '/logo192.png', '/logo512.png', '/favicon.ico'];

sw.addEventListener('install', (e) => {
  e.waitUntil(
    caches
      .open(CACHE)
      .then((c) => c.addAll(SHELL))
      .then(() => sw.skipWaiting()),
  );
});

sw.addEventListener('activate', (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => sw.clients.claim()),
  );
});

sw.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== sw.location.origin || url.pathname.startsWith('/api/')) return;
  if (e.request.mode === 'navigate') {
    e.respondWith(
      fetch(e.request)
        .then((r) => {
          const copy = r.clone();
          e.waitUntil(caches.open(CACHE).then((c) => c.put('/', copy)));
          return r;
        })
        .catch(async () => (await caches.match('/')) ?? Response.error()),
    );
    return;
  }
  if (url.pathname.startsWith('/_expo/') || SHELL.includes(url.pathname)) {
    e.respondWith(
      caches.match(e.request).then(
        (hit) =>
          hit ||
          fetch(e.request).then((r) => {
            const copy = r.clone();
            e.waitUntil(caches.open(CACHE).then((c) => c.put(e.request, copy)));
            return r;
          }),
      ),
    );
  }
});
