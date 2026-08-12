const updateAvailableEvent = "thoughtglean:update-available";
let waitingWorker: ServiceWorker | undefined;
let updateAnnounced = false;

function announceUpdate(worker?: ServiceWorker) {
  if (worker) waitingWorker = worker;
  if (updateAnnounced) return;
  updateAnnounced = true;
  dispatchEvent(new CustomEvent(updateAvailableEvent));
}

async function checkBuildVersion() {
  const current = document.querySelector<HTMLMetaElement>('meta[name="thoughtglean-build"]')?.content;
  if (!current) return;
  try {
    const response = await fetch(`/?pwa-version-check=${Date.now()}`, { cache: "no-store" });
    if (!response.ok) return;
    const html = await response.text();
    const latest = new DOMParser().parseFromString(html, "text/html").querySelector<HTMLMetaElement>('meta[name="thoughtglean-build"]')?.content;
    if (latest && latest !== current) announceUpdate();
  } catch { /* Offline is expected; check again after reconnecting. */ }
}

export function applyPWAUpdate() {
  if (waitingWorker) waitingWorker.postMessage({ type: "SKIP_WAITING" });
  else location.reload();
}

export const pwaUpdateEvent = updateAvailableEvent;

if ("serviceWorker" in navigator) {
  if (import.meta.env.DEV) {
    // Development must never retain a production shell for stable /src URLs.
    void navigator.serviceWorker.getRegistrations().then(registrations => Promise.all(registrations.map(registration => registration.unregister())));
    void caches.keys().then(keys => Promise.all(keys.filter(key => key.startsWith("thoughtglean-")).map(key => caches.delete(key))));
  } else {
    let refreshing = false;
    navigator.serviceWorker.addEventListener("controllerchange", () => {
      if (!refreshing) { refreshing = true; location.reload(); }
    });
    addEventListener("load", () => void navigator.serviceWorker.register("/sw.js").then(registration => {
      if (registration.waiting) announceUpdate(registration.waiting);
      registration.addEventListener("updatefound", () => {
        const worker = registration.installing;
        worker?.addEventListener("statechange", () => {
          if (worker.state === "installed" && navigator.serviceWorker.controller) announceUpdate(worker);
        });
      });
      void checkBuildVersion();
      window.setInterval(() => { void registration.update(); void checkBuildVersion(); }, 15 * 60_000);
    }).catch(() => undefined));
    addEventListener("online", () => void checkBuildVersion());
    document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") void checkBuildVersion(); });
  }
}
