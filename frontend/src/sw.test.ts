import { expect, it, vi } from "vitest";
import swSource from "../public/sw.js?raw";

it("clones a network response before returning it to the page", async () => {
  const listeners = new Map<string, (event: unknown) => void>();
  const worker = {
    location: { origin: "https://notes.example" },
    clients: { claim: vi.fn() },
    skipWaiting: vi.fn(),
    addEventListener: (type: string, listener: (event: unknown) => void) => listeners.set(type, listener),
  };
  const put = vi.fn(async (_key: unknown, response: Response) => {
    expect(await response.text()).toBe("asset contents");
  });
  let releaseCache: ((cache: { put: typeof put }) => void) | undefined;
  const cacheReady = new Promise<{ put: typeof put }>(resolve => { releaseCache = resolve; });
  const cacheStorage = {
    has: vi.fn(async () => false),
    keys: vi.fn(async () => []),
    delete: vi.fn(async () => true),
    match: vi.fn(async () => undefined),
    open: vi.fn(() => cacheReady),
  };
  const networkFetch = vi.fn(async () => new Response("asset contents"));

  new Function("self", "caches", "fetch", "Response", "URL", "indexedDB", "File", swSource)(
    worker, cacheStorage, networkFetch, Response, URL, {}, File,
  );

  let delivered: Promise<Response> | undefined;
  listeners.get("fetch")?.({
    request: { url: "https://notes.example/assets/app.js", method: "GET", mode: "same-origin" },
    respondWith: (response: Promise<Response>) => { delivered = response; },
  });
  const pageResponse = await delivered;
  expect(await pageResponse?.text()).toBe("asset contents");

  releaseCache?.({ put });
  await vi.waitFor(() => expect(put).toHaveBeenCalledOnce());
});
