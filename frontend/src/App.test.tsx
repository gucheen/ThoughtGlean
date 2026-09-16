import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EditorView } from "@codemirror/view";
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
  await db.transaction("rw", [db.notes, db.sources, db.materialLinks, db.verifications, db.topics, db.topicMemberships, db.attachments, db.events, db.metadata], async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.materialLinks.clear(), db.verifications.clear(), db.topics.clear(), db.topicMemberships.clear(), db.attachments.clear(), db.events.clear(), db.metadata.clear()]);
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
    await userEvent.click(screen.getByText("当时的记录", { selector: "summary" }));
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

  it("saves shared text as an ordinary note with its URL, without touching existing notes", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const existing = noteFixture("existing", "已有操作", now(), { kind: "procedure" });
    await db.notes.put(existing);
    await writeLibraryMetadata("share.pending.v1", [{ id: "share", title: "随手分享", text: "今天看到的一段话", url: "https://example.com/source", createdAt: now() }]);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "收到的分享" }));
    await userEvent.click(await screen.findByRole("button", { name: "保存记录" }));
    await waitFor(async () => expect(await db.notes.count()).toBe(2));
    const note = (await db.notes.toArray()).find(item => item.id !== existing.id)!;
    expect(note).toMatchObject({ kind: "note", title: "随手分享", content: "今天看到的一段话\n\nhttps://example.com/source" });
    expect(await db.notes.get(existing.id)).toEqual(existing);
    expect(await db.materialLinks.count()).toBe(0);
    expect(await db.verifications.count()).toBe(0);
    expect((await db.metadata.get("share.pending.v1"))?.value).toEqual([]);
    expect(await screen.findByRole("heading", { name: "随手分享" })).toBeInTheDocument();
  });


  it("finds old materials through search, all records, and the trash", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const material = noteFixture("material", "旧对话素材", now(), { kind: "material" });
    const deleted = noteFixture("deleted-material", "已删除素材", now(), { kind: "material", deletedAt: now() });
    await db.notes.bulkPut([material, deleted]);
    render(<App />);
    const search = await screen.findByRole("searchbox");
    expect(screen.queryByText(material.title)).not.toBeInTheDocument();
    await userEvent.type(search, "旧对话");
    expect(await screen.findByRole("button", { name: /旧对话素材/ })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "设置" }));
    await userEvent.click(screen.getByRole("button", { name: "全部记录" }));
    expect(await screen.findByText(material.title)).toBeInTheDocument();
    expect(search).toHaveValue("");
    await userEvent.click(screen.getByRole("button", { name: "设置" }));
    await userEvent.click(screen.getByRole("button", { name: "回收站" }));
    expect(await screen.findByText(deleted.title)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "恢复" }));
    await waitFor(async () => expect((await db.notes.get(deleted.id))?.deletedAt).toBeUndefined());
  });


  it("preserves old verification history behind a disclosure without maintenance actions", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("procedure", "旧操作", now(), { kind: "procedure", revision: 2 });
    await db.notes.put(note);
    const verification = { id: "verification", syncId: "sync-verification", noteId: note.id, noteRevision: 1, verifiedAt: now(), environment: "Ubuntu 24.04", result: "success" as const, comment: "服务正常" };
    await db.verifications.put(verification);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    const disclosure = await screen.findByText("以往的使用记录");
    expect(disclosure.closest("details")).not.toHaveAttribute("open");
    expect(screen.queryByRole("button", { name: "记录一次使用" })).not.toBeInTheDocument();
    await userEvent.click(disclosure);
    expect(screen.getByText("Ubuntu 24.04")).toBeVisible();
    expect(screen.getByText("正文更新后待重新验证")).toBeVisible();
    expect(await db.verifications.get(verification.id)).toEqual(verification);
  });


  it("keeps capture first and starts new notes outside search and star filters", async () => {
    await db.notes.put(noteFixture("old", "旧操作", now(), { kind: "procedure" }));
    render(<App />);
    await screen.findByRole("textbox", { name: "随手记" });
    expect(screen.queryByRole("button", { name: "主题" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "已验证" })).not.toBeInTheDocument();
    expect(screen.queryByText("未实际验证")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "星标" }));
    await userEvent.type(screen.getByRole("searchbox"), "不会命中");
    await userEvent.click(screen.getByRole("button", { name: /新记录/ }));
    expect(screen.getByRole("searchbox")).toHaveValue("");
    expect(window.location.pathname).toBe("/");
    const capture = screen.getByRole("textbox", { name: "随手记" });
    fireEvent.change(capture, { target: { value: "只记一句话" } });
    await userEvent.click(screen.getByRole("button", { name: "保存" }));
    expect(await screen.findByText("只记一句话", { selector: ".note-row-title" })).toBeInTheDocument();
  });


  it("shows and copies a matching command directly from search results", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    await db.notes.put(noteFixture("command", "查看 Docker 磁盘占用", now(), { kind: "procedure", content: "```bash\ndocker system df -v\n```" }));
    render(<App />);

    await userEvent.type(await screen.findByRole("searchbox"), "docker system df");
    await userEvent.click(await screen.findByRole("button", { name: "复制命令" }));
    expect(writeText).toHaveBeenCalledWith("docker system df -v");
  });

  it("appends sharing to a recoverable home draft without replacing existing text", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    await writeLibraryMetadata("draft.home.v1", { content: "没写完的想法" });
    await writeLibraryMetadata("share.pending.v1", [{ id: "share", title: "", text: "补充一句", url: "https://example.com", createdAt: now() }]);
    const app = render(<App />);
    expect(await screen.findByRole("textbox", { name: "随手记" })).toHaveValue("没写完的想法");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "收到的分享" }));
    await userEvent.click(await screen.findByRole("button", { name: "加入草稿" }));
    const content = "没写完的想法\n\n补充一句\n\nhttps://example.com";
    expect(screen.getByRole("textbox", { name: "随手记" })).toHaveValue(content);
    expect((await db.metadata.get("draft.home.v1"))?.value).toMatchObject({ content });
    expect((await db.metadata.get("share.pending.v1"))?.value).toEqual([]);
    app.unmount();
    render(<App />);
    expect(await screen.findByRole("textbox", { name: "随手记" })).toHaveValue(content);
    expect(await db.notes.count()).toBe(0);
  });

  it("retains a pending share and rolls back the note when saving fails", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    await writeLibraryMetadata("share.pending.v1", [{ id: "share", title: "", text: "不能丢的分享", url: "", createdAt: now() }]);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "收到的分享" }));
    const save = await screen.findByRole("button", { name: "保存记录" });
    const failure = vi.spyOn(db.metadata, "put").mockRejectedValueOnce(new Error("disk full"));
    await userEvent.click(save);
    expect(await screen.findByRole("alert")).toHaveTextContent("分享内容仍然保留");
    failure.mockRestore();
    expect(await db.notes.count()).toBe(0);
    expect((await db.metadata.get("share.pending.v1"))?.value).toHaveLength(1);
    await userEvent.click(save);
    await waitFor(async () => expect(await db.notes.count()).toBe(1));
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
    await userEvent.click(screen.getByText("当时的记录", { selector: "summary" }));
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

  it("keeps the same editor and selection when switching between live preview and source", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const content = "## 标题\n\n这是一段 **需要保留** 的正文\n\n```js\nconst value = 1;\n```";
    const note = noteFixture("editor", "编辑体验", now(), { content });
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    const noteActions = await screen.findByRole("button", { name: "更多" });
    expect(noteActions).toHaveClass("mobile-note-actions-toggle");
    expect(noteActions).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(noteActions);
    expect(noteActions).toHaveAttribute("aria-expanded", "true");
    fireEvent.pointerDown(document.body);
    expect(noteActions).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const element = await screen.findByRole("textbox", { name: "正文编辑器" });
    const editor = EditorView.findFromDOM(element)!;
    expect(document.querySelector(".app-shell")).toHaveClass("is-detail-editing");
    const toolsToggle = screen.getByRole("button", { name: "工具" });
    expect(toolsToggle).toHaveAttribute("aria-expanded", "false");
    expect(editor.state.doc.toString()).toBe(content);
    act(() => editor.dispatch({ selection: { anchor: 12, head: 16 } }));
    await userEvent.click(toolsToggle);
    expect(toolsToggle).toHaveAttribute("aria-expanded", "true");
    await userEvent.click(screen.getByRole("button", { name: "Markdown 源码" }));
    expect(toolsToggle).toHaveAttribute("aria-expanded", "false");
    expect(EditorView.findFromDOM(element)).toBe(editor);
    expect(editor.state.selection.main).toMatchObject({ anchor: 12, head: 16 });
    await userEvent.click(screen.getByRole("button", { name: "返回实时预览" }));
    expect(editor.state.doc.toString()).toBe(content);
    await userEvent.click(toolsToggle);
    fireEvent.pointerDown(document.body);
    expect(toolsToggle).toHaveAttribute("aria-expanded", "false");
    const actions = screen.getByRole("toolbar", { name: "编辑操作" });
    expect(within(actions).getByRole("button", { name: "插入代码片段" })).toBeInTheDocument();
    expect(within(actions).getByText("添加图片")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "保存修改" }));
    expect(await screen.findByRole("button", { name: "编辑" })).toBeInTheDocument();
    expect(document.querySelector(".app-shell")).not.toHaveClass("is-detail-editing");
    expect(await db.notes.get(note.id)).toMatchObject({ content, revision: 1 });
  });

  it.each(["button", "shortcut"])("only enters editing explicitly via %s and carries a reading selection into the source document", async entry => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const content = "普通正文\n\n**选中的文字**\n\n```text\n代码区\n```";
    const note = noteFixture("selection", "可选择的标题", now(), { content });
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    await userEvent.click(await screen.findByRole("heading", { name: note.title }));
    await userEvent.dblClick(screen.getByText("普通正文"));
    await userEvent.click(screen.getByText("代码区"));
    expect(screen.queryByRole("textbox", { name: "正文编辑器" })).not.toBeInTheDocument();
    const text = screen.getByText("选中的文字").firstChild!;
    window.getSelection()!.setBaseAndExtent(text, 1, text, 4);
    await waitFor(() => expect(screen.getByRole("button", { name: "编辑" })).toBeEnabled());
    if (entry === "button") fireEvent.click(screen.getByRole("button", { name: "编辑" }));
    else expect(fireEvent.keyDown(document.body, { key: "e" })).toBe(false);
    const element = await screen.findByRole("textbox", { name: "正文编辑器" });
    const editor = EditorView.findFromDOM(element)!;
    expect(element).toHaveFocus();
    expect(editor.state.sliceDoc()).toBe(content);
    expect(editor.state.selection.main).toMatchObject({ anchor: content.indexOf("选中的文字") + 1, head: content.indexOf("选中的文字") + 4 });
  });

  it("ignores the edit shortcut while typing, composing, holding modifiers, or using a dialog", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("shortcut", "快捷编辑", now());
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    await waitFor(() => expect(screen.getByRole("button", { name: "编辑" })).toBeEnabled());
    for (const options of [{ ctrlKey: true }, { metaKey: true }, { altKey: true }, { shiftKey: true }, { repeat: true }, { isComposing: true }, { keyCode: 229 }]) {
      expect(fireEvent.keyDown(document.body, { key: "e", ...options })).toBe(true);
      expect(screen.queryByRole("textbox", { name: "正文编辑器" })).not.toBeInTheDocument();
    }
    const handled = new KeyboardEvent("keydown", { key: "e", bubbles: true, cancelable: true });
    handled.preventDefault();
    fireEvent(document.body, handled);
    fireEvent.keyDown(screen.getByRole("searchbox"), { key: "e" });
    expect(screen.queryByRole("textbox", { name: "正文编辑器" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "设置" }));
    fireEvent.keyDown(screen.getByRole("button", { name: "关闭" }), { key: "e" });
    expect(screen.queryByRole("textbox", { name: "正文编辑器" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "关闭" }));
    fireEvent.keyDown(document.body, { key: "E" });
    const element = await screen.findByRole("textbox", { name: "正文编辑器" });
    const editor = EditorView.findFromDOM(element)!;
    act(() => editor.dispatch({ changes: { from: 0, insert: "修改" } }));
    fireEvent.keyDown(element, { key: "e" });
    expect(editor.state.sliceDoc()).toBe(`修改${note.content}`);
    expect(EditorView.findFromDOM(element)).toBe(editor);
  });

  it("preserves a changed draft when immediately navigating away and restores it on return", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("draft-editor", "未完成的修改", now());
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const editor = EditorView.findFromDOM(await screen.findByRole("textbox", { name: "正文编辑器" }))!;
    act(() => editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: "**保留这份草稿**" } }));
    fireEvent.click(screen.getByRole("button", { name: "← 返回" }));
    await userEvent.click(await screen.findByRole("button", { name: new RegExp(note.title) }));
    await userEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const restored = EditorView.findFromDOM(await screen.findByRole("textbox", { name: "正文编辑器" }))!;
    expect(restored.state.sliceDoc()).toBe("**保留这份草稿**");
    expect((await db.notes.get(note.id))?.content).toBe(note.content);
    await userEvent.click(screen.getByRole("button", { name: "保存修改" }));
    await waitFor(async () => expect((await db.notes.get(note.id))?.content).toBe("**保留这份草稿**"));
    expect(await screen.findByRole("button", { name: "编辑" })).toBeInTheDocument();
  });

  it("keeps edits when cancellation is declined and discards them only after confirmation", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("cancel-editor", "取消修改", now());
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "编辑" }));
    fireEvent.change(screen.getByRole("textbox", { name: "记录标题" }), { target: { value: "修改后的标题" } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    try {
      await userEvent.click(screen.getByRole("button", { name: "取消" }));
      expect(screen.getByRole("textbox", { name: "记录标题" })).toHaveValue("修改后的标题");
      confirm.mockReturnValue(true);
      await userEvent.click(screen.getByRole("button", { name: "取消" }));
      expect(await screen.findByRole("heading", { name: note.title })).toBeInTheDocument();
      expect((await db.notes.get(note.id))?.title).toBe(note.title);
    } finally { confirm.mockRestore(); }
  });

  it("does not offer editing for a trashed note", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("deleted-editor", "已删除记录", now(), { deletedAt: now() });
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    await screen.findByRole("heading", { name: note.title });
    expect(screen.queryByRole("button", { name: "编辑" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "复制 Markdown" })).toBeInTheDocument();
    fireEvent.keyDown(document.body, { key: "e" });
    expect(screen.queryByRole("textbox", { name: "正文编辑器" })).not.toBeInTheDocument();
  });

  it("keeps the editor and unsaved text open when local saving fails", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("failed-editor", "保存失败测试", now());
    await saveNote(note, false);
    history.replaceState(null, "", `/notes/${note.id}`);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const editor = EditorView.findFromDOM(await screen.findByRole("textbox", { name: "正文编辑器" }))!;
    act(() => editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: "不能丢失的修改" } }));
    const put = vi.spyOn(db.notes, "put").mockRejectedValueOnce(new Error("磁盘不可写"));
    try {
      await userEvent.click(screen.getByRole("button", { name: "保存修改" }));
      expect(await screen.findByRole("alert")).toHaveTextContent("保存失败：磁盘不可写");
      expect(EditorView.findFromDOM(screen.getByRole("textbox", { name: "正文编辑器" }))).toBe(editor);
      expect(editor.state.sliceDoc()).toBe("不能丢失的修改");
      expect((await db.notes.get(note.id))?.content).toBe(note.content);
    } finally { put.mockRestore(); }
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

  it("keeps existing topic links available through settings without organization controls", async () => {
    Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
    const note = noteFixture("topic-note", "旧主题里的记录", now());
    const topic = { id: "topic", syncId: "sync-topic", name: "服务器管理", createdAt: now(), updatedAt: now() };
    const membership = { id: "membership", syncId: "sync-membership", topicId: topic.id, noteId: note.id, pinned: true, createdAt: now(), updatedAt: now() };
    await db.notes.put(note); await db.topics.put(topic); await db.topicMemberships.put(membership);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "设置" }));
    await userEvent.click(screen.getByRole("button", { name: "以往的主题" }));
    await userEvent.click(await screen.findByRole("button", { name: /服务器管理/ }));
    await userEvent.click(await screen.findByRole("button", { name: /旧主题里的记录/ }));
    expect(await screen.findByRole("heading", { name: note.title })).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "加入主题" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByText("以往的归类与来源"));
    await userEvent.click(screen.getByRole("button", { name: topic.name }));
    expect(await screen.findByRole("heading", { name: topic.name })).toBeInTheDocument();
    expect(await db.topicMemberships.get(membership.id)).toEqual(membership);
  });


});
