if ("serviceWorker" in navigator) {
  if (import.meta.env.DEV) {
    // Development must never retain a production shell for stable /src URLs.
    void navigator.serviceWorker.getRegistrations().then(registrations => Promise.all(registrations.map(registration => registration.unregister())));
    void caches.keys().then(keys => Promise.all(keys.filter(key => key.startsWith("thoughtglean-")).map(key => caches.delete(key))));
  } else {
    addEventListener("load", () => navigator.serviceWorker.register("/sw.js").catch(() => undefined));
  }
}
