const CACHE = "thoughtglean-shell-v4";
const LEGACY_CACHE = "thoughtglean-shell-v3";
const SHARE_DB = "thoughtglean-share-v1";

function saveShare(payload) {
  return new Promise((resolve, reject) => {
    const open = indexedDB.open(SHARE_DB, 1);
    open.onupgradeneeded = () => open.result.createObjectStore("inbox", { keyPath: "id", autoIncrement: true });
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const transaction = open.result.transaction("inbox", "readwrite");
      transaction.objectStore("inbox").add(payload);
      transaction.oncomplete = () => { open.result.close(); resolve(); };
      transaction.onerror = () => reject(transaction.error);
    };
  });
}

async function receiveShare(request) {
  const data = await request.formData();
  await saveShare({
    title: String(data.get("title") || ""),
    text: String(data.get("text") || ""),
    url: String(data.get("url") || ""),
    images: data.getAll("images").filter(value => value instanceof File && value.type.startsWith("image/")),
    createdAt: new Date().toISOString(),
  });
  return Response.redirect("/share", 303);
}

function cacheSuccessfulResponse(key, response) {
  if (!response.ok) return;
  const copy = response.clone();
  void caches.open(CACHE).then(cache => cache.put(key, copy)).catch(() => undefined);
}

self.addEventListener("install", event => event.waitUntil(
  caches.has(LEGACY_CACHE).then(upgradingLegacy => upgradingLegacy ? self.skipWaiting() : undefined),
));
self.addEventListener("message", event => {
  if (event.data?.type === "SKIP_WAITING") void self.skipWaiting();
});
self.addEventListener("activate", event => event.waitUntil(
  caches.keys().then(keys => Promise.all(keys.filter(key => key.startsWith("thoughtglean-") && key !== CACHE).map(key => caches.delete(key)))).then(() => self.clients.claim()),
));
self.addEventListener("fetch", event => {
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith("/api/")) return;
  if (event.request.method === "POST" && url.pathname === "/share") {
    event.respondWith(receiveShare(event.request));
    return;
  }
  if (event.request.method !== "GET") return;
  if (url.pathname === "/" || event.request.mode === "navigate") {
    event.respondWith(fetch(event.request).then(response => {
      cacheSuccessfulResponse("/", response);
      return response;
    }).catch(() => caches.match(event.request).then(hit => hit || caches.match("/"))));
    return;
  }
  event.respondWith(caches.match(event.request).then(hit => hit || fetch(event.request).then(response => {
    cacheSuccessfulResponse(event.request, response);
    return response;
  })));
});
