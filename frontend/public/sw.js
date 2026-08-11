const CACHE = "thoughtglean-shell-v3";
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
self.addEventListener("install", event => event.waitUntil(self.skipWaiting()));
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
  event.respondWith(caches.match(event.request).then(hit => hit || fetch(event.request).then(response => {
    if (response.ok) void caches.open(CACHE).then(cache => cache.put(event.request, response.clone()));
    return response;
  })));
});
