import "fake-indexeddb/auto";
import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => cleanup());

Object.defineProperty(navigator, "onLine", { configurable: true, value: true });
Object.defineProperty(navigator, "storage", { configurable: true, value: { persist: vi.fn().mockResolvedValue(true) } });
Object.defineProperty(window, "scrollTo", { configurable: true, value: vi.fn() });
Object.defineProperty(window, "scrollBy", { configurable: true, value: vi.fn() });
Object.defineProperty(Range.prototype, "getClientRects", { configurable: true, value: () => [] });
Object.defineProperty(Range.prototype, "getBoundingClientRect", { configurable: true, value: () => new DOMRect() });
Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:test") });
Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
