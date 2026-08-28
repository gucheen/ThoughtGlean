import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";
import { db, now, saveNote, writeLibraryMetadata, type Note } from "./db";

const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200, headers: { "Content-Type": "application/json" } });

const noteFixture = (id: string, title: string, createdAt: string, patch: Partial<Note> = {}): Note => ({
  id, syncId: `sync-${id}`, title, content: `${title}的正文`, starred: false, revision: 1, createdAt, updatedAt: createdAt, ...patch,
});

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
  it("separates direct continuations from temporal neighbors and navigates in both directions", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const root = noteFixture("root", "最初的想法", "2026-01-01T00:00:00Z");
    const child = noteFixture("child", "几个月后的续写", "2026-06-01T00:00:00Z", { continuedFromId: root.id });
    const sibling = noteFixture("sibling", "另一个方向", "2026-07-01T00:00:00Z", { continuedFromId: root.id });
    const grandchild = noteFixture("grandchild", "继续深入", "2026-08-01T00:00:00Z", { continuedFromId: child.id });
    const conflict = noteFixture("conflict", "同步冲突：最初的想法", "2026-06-01T01:00:00Z", { continuedFromId: root.id });
    const removed = noteFixture("removed", "已删除的续写", "2026-06-01T02:00:00Z", { continuedFromId: root.id, deletedAt: now() });
    const neighbors = [1, 2, 3].map(day => noteFixture(`neighbor-${day}`, `当时的其他记录 ${day}`, `2026-05-0${day}T00:00:00Z`));
    await db.notes.bulkPut([sibling, grandchild, root, child, conflict, removed, ...neighbors]);
    history.replaceState(null, "", `/notes/${child.id}`);
    render(<App />);

    const relations = await screen.findByRole("region", { name: "续写关系" });
    expect(within(relations).getByRole("button", { name: /最初的想法/ })).toBeInTheDocument();
    expect(within(relations).getByRole("button", { name: /继续深入/ })).toBeInTheDocument();
    expect(within(relations).queryByText("另一个方向")).not.toBeInTheDocument();
    const timeline = screen.getByRole("region", { name: "当时的记录" });
    expect(within(timeline).queryByText(root.title)).not.toBeInTheDocument();
    expect(within(timeline).getByText("当时的其他记录 3")).toBeInTheDocument();

    await userEvent.click(within(relations).getByRole("button", { name: /最初的想法/ }));
    expect(await screen.findByRole("heading", { level: 1, name: root.title })).toBeInTheDocument();
    const rootRelations = screen.getByRole("region", { name: "续写关系" });
    expect(within(rootRelations).getByRole("heading", { name: "后续续写2" })).toBeInTheDocument();
    expect(within(rootRelations).getAllByRole("button").map(button => button.textContent)).toEqual([
      expect.stringContaining(child.title), expect.stringContaining(sibling.title), "继续写",
    ]);
    await userEvent.click(within(rootRelations).getByRole("button", { name: /另一个方向/ }));
    expect(await screen.findByRole("heading", { level: 1, name: sibling.title })).toBeInTheDocument();
  });

  it("saves a continuation with a visible source and allows returning to the original", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const original = noteFixture("capture-parent", "需要接着想的记录", "2026-01-01T00:00:00Z");
    await saveNote(original, false);
    history.replaceState(null, "", `/notes/${original.id}`);
    const app = render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "继续写" }));
    const editor = await screen.findByPlaceholderText("继续写…");
    expect(screen.getByRole("button", { name: original.title })).toBeInTheDocument();
    fireEvent.change(editor, { target: { value: "接着想出的新内容" } });
    await userEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(async () => expect(await db.notes.count()).toBe(2));
    const child = (await db.notes.toArray()).find(note => note.id !== original.id)!;
    expect(child.continuedFromId).toBe(original.id);
    expect(await screen.findByText(`续写自：${original.title}`)).toBeInTheDocument();
    expect(screen.getByText("1 条后续续写")).toBeInTheDocument();
    expect(await db.notes.get(original.id)).toMatchObject({ content: original.content, revision: 1 });

    app.unmount();
    history.replaceState(null, "", `/notes/${child.id}`);
    render(<App />);
    const relations = await screen.findByRole("region", { name: "续写关系" });
    await userEvent.click(within(relations).getByRole("button", { name: new RegExp(original.title) }));
    expect(await screen.findByRole("heading", { level: 1, name: original.title })).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "续写关系" })).getByRole("button", { name: /接着想出的新内容/ })).toBeInTheDocument();
  });

  it("keeps relation labels when the parent or children are filtered out", async () => {
    const original = noteFixture("filtered-parent", "原始想法", "2026-01-01T00:00:00Z");
    const child = noteFixture("filtered-child", "独特的后续", "2026-06-01T00:00:00Z", { continuedFromId: original.id });
    await db.notes.bulkPut([original, child]);
    render(<App />);
    const search = await screen.findByRole("searchbox");
    await userEvent.type(search, child.title);
    expect(await screen.findByText(`续写自：${original.title}`)).toBeInTheDocument();
    expect(screen.queryByText(original.title, { selector: ".note-row-title" })).not.toBeInTheDocument();
    await userEvent.clear(search);
    await userEvent.type(search, original.title);
    expect(screen.getByText("1 条后续续写")).toBeInTheDocument();
    expect(screen.queryByText(child.title, { selector: ".note-row-title" })).not.toBeInTheDocument();
  });

  it("restores the continuation source with a draft and can cancel the link without discarding text", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const original = noteFixture("draft-parent", "草稿的原记录", "2026-01-01T00:00:00Z");
    await saveNote(original, false);
    await writeLibraryMetadata("draft.home.v1", { content: "保留这段草稿", continuedFromID: original.id });
    render(<App />);
    expect(await screen.findByRole("button", { name: original.title })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("继续写…")).toHaveValue("保留这段草稿");
    await userEvent.click(screen.getByRole("button", { name: "取消续写" }));
    expect(screen.getByPlaceholderText("写下想法…")).toHaveValue("保留这段草稿");
    await userEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(async () => expect(await db.notes.count()).toBe(2));
    expect((await db.notes.toArray()).find(note => note.id !== original.id)?.continuedFromId).toBeUndefined();
  });

  it("shows a deleted source and preserves the relation after restoring it", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const original = noteFixture("deleted-parent", "回收站里的原记录", "2026-01-01T00:00:00Z", { deletedAt: now() });
    const child = noteFixture("active-child", "保留的续写", "2026-06-01T00:00:00Z", { continuedFromId: original.id });
    await db.notes.bulkPut([original, child]);
    history.replaceState(null, "", `/notes/${child.id}`);
    render(<App />);
    const relations = await screen.findByRole("region", { name: "续写关系" });
    expect(within(relations).getByText("已在回收站")).toBeInTheDocument();
    await userEvent.click(within(relations).getByRole("button", { name: /回收站里的原记录/ }));
    const timeline = screen.getByRole("region", { name: "当时的记录" });
    expect(within(timeline).getByRole("button", { name: /回收站里的原记录/ })).toHaveAttribute("aria-current", "true");
    expect(screen.queryByRole("button", { name: "继续写" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "恢复" }));
    await waitFor(async () => expect((await db.notes.get(original.id))?.deletedAt).toBeUndefined());
    expect((await db.notes.get(child.id))?.continuedFromId).toBe(original.id);
  });

  it("explains an unavailable source without treating a temporal neighbor as the parent", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const child = noteFixture("orphan", "来源暂不可用的续写", "2026-06-01T00:00:00Z", { continuedFromId: "missing" });
    await db.notes.bulkPut([child, noteFixture("neighbor", "相邻记录", "2026-05-31T00:00:00Z")]);
    history.replaceState(null, "", `/notes/${child.id}`);
    render(<App />);
    const relations = await screen.findByRole("region", { name: "续写关系" });
    expect(within(relations).getByText("原记录暂不可用，关联仍保留。")).toBeInTheDocument();
    expect(within(relations).queryByRole("button", { name: /相邻记录/ })).not.toBeInTheDocument();
  });

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

  it("grows the detail editor with its content instead of scrolling internally", async () => {
    const originalScrollHeight = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "scrollHeight");
    Object.defineProperty(HTMLTextAreaElement.prototype, "scrollHeight", {
      configurable: true,
      get() { return this.value.length > 40 ? 720 : 180; },
    });

    try {
      const timestamp = now();
      const content = "这是一段很长的正文".repeat(20);
      await saveNote({ id: "note-long-editor", syncId: "sync-long-editor-123456", title: "长记录", content, starred: false, revision: 1, createdAt: timestamp, updatedAt: timestamp }, false);
      history.replaceState(null, "", "/notes/note-long-editor");
      render(<App />);

      await userEvent.click(await screen.findByRole("button", { name: "编辑正文" }));
      const editor = await screen.findByDisplayValue(content);
      await waitFor(() => expect(editor).toHaveStyle({ height: "720px", overflowY: "hidden" }));
      const actions = screen.getByRole("toolbar", { name: "编辑操作" });
      expect(actions).toContainElement(screen.getByRole("button", { name: "插入代码片段" }));
      expect(actions).toContainElement(screen.getByText("添加图片"));
      expect(actions).toContainElement(screen.getByRole("button", { name: "取消" }));
      expect(actions).toContainElement(screen.getByRole("button", { name: "保存修改" }));

      fireEvent.change(editor, { target: { value: "较短的正文" } });
      await waitFor(() => expect(editor).toHaveStyle({ height: "180px", overflowY: "hidden" }));
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
