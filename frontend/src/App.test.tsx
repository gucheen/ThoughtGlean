import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";
import { db, now, saveNote, type Note } from "./db";

const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200, headers: { "Content-Type": "application/json" } });

beforeEach(async () => {
  Object.defineProperty(navigator, "onLine", { configurable: true, value: true });
  history.replaceState(null, "", "/");
  localStorage.clear();
  await db.open();
  await db.transaction("rw", db.notes, db.sources, db.attachments, db.events, db.metadata, async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.attachments.clear(), db.events.clear(), db.metadata.clear()]);
  });
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    const path = typeof input === "string" ? input : input.toString();
    if (path === "/api/auth/status") return json({ enabled: false, configured: false, authenticated: true, tokenLoginEnabled: true });
    if (path === "/api/health") return json({ status: "ok", version: "server-test" });
    if (path === "/api/sync/snapshot") return json({ generatedAt: now(), notes: await db.notes.toArray(), sources: [], attachments: [] });
    return json({});
  }));
});

describe("core note interactions", () => {
  it("grows the capture textarea until its scroll limit", async () => {
    const originalScrollHeight = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "scrollHeight");
    Object.defineProperty(HTMLTextAreaElement.prototype, "scrollHeight", {
      configurable: true,
      get() { return this.value.length > 80 ? 500 : 120; },
    });

    try {
      render(<App />);
      const editor = await screen.findByPlaceholderText("写下想法…");
      await waitFor(() => expect(editor).toHaveStyle({ height: "120px", overflowY: "hidden" }));

      fireEvent.change(editor, { target: { value: `# 长标题\n${"较长的正文".repeat(30)}` } });
      await waitFor(() => expect(editor).toHaveStyle({ height: "320px", overflowY: "auto" }));
      expect(editor.closest("form")).toHaveClass("has-title-line");
    } finally {
      if (originalScrollHeight) Object.defineProperty(HTMLTextAreaElement.prototype, "scrollHeight", originalScrollHeight);
      else Reflect.deleteProperty(HTMLTextAreaElement.prototype, "scrollHeight");
    }
  });

  it("selects search results with the keyboard and opens with Enter", async () => {
    const timestamp = now();
    await saveNote({ id: "note-search-a", syncId: "sync-search-a-1234567890", title: "普通记录", content: "没有目标", starred: false, revision: 1, createdAt: timestamp, updatedAt: timestamp }, false);
    await saveNote({ id: "note-search-b", syncId: "sync-search-b-1234567890", title: "寻找月光", content: "关键词在这里", starred: false, revision: 1, createdAt: timestamp, updatedAt: timestamp }, false);
    render(<App />);
    const search = await screen.findByRole("searchbox");
    await userEvent.type(search, "月光");
    fireEvent.keyDown(search, { key: "Enter" });
    expect(await screen.findByRole("heading", { name: "寻找月光" })).toBeInTheDocument();
  });

  it("resolves a conflict by keeping the current version", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const timestamp = now();
    const current: Note = { id: "note-current", syncId: "sync-current-1234567890", title: "当前版本", content: "当前正文", starred: false, revision: 2, createdAt: timestamp, updatedAt: timestamp };
    const conflict: Note = { id: "note-conflict", syncId: "sync-conflict-123456789", title: "同步冲突：当前版本", content: "另一正文", starred: false, continuedFromId: current.id, revision: 1, createdAt: timestamp, updatedAt: timestamp };
    await saveNote(current, false); await saveNote(conflict, false);
    history.replaceState(null, "", `/notes/${conflict.id}`);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "保留当前" }));
    await waitFor(async () => expect((await db.notes.get(conflict.id))?.deletedAt).toBeTruthy());
    expect(await screen.findByRole("heading", { name: "当前版本" })).toBeInTheDocument();
  });

  it("shows local and server diagnostics in settings", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const timestamp = now();
    await saveNote({ id: "note-diagnostic", syncId: "sync-diagnostic-123456789", title: "诊断记录", content: "正文", starred: false, revision: 1, createdAt: timestamp, updatedAt: timestamp }, false);
    await db.attachments.put({ id: "image-diagnostic", syncId: "image-sync-diagnostic", noteId: "note-diagnostic", originalName: "photo.png", mimeType: "image/png", byteSize: 3, createdAt: timestamp, blob: new Blob(["png"], { type: "image/png" }) });

    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "设置" }));
    const dialog = await screen.findByRole("dialog", { name: "设置" });
    expect(dialog).toHaveTextContent("待同步操作");
    expect(dialog).toHaveTextContent("1 条记录 · 1 张图片");
    expect(await screen.findByText("server-test")).toBeInTheDocument();
  });
});
