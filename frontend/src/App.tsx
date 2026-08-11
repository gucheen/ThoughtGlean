import { useEffect, useMemo, useRef, useState, type ChangeEvent, type ClipboardEvent } from "react";
import { useLiveQuery } from "dexie-react-hooks";
import { createNote, db, exportBackup, markdownExport, migrateLegacyLibrary, newID, now, queue, restoreBackup, saveNote, type Attachment, type Note } from "./db";
import { pairingCode, parsePairingCode, queueAttachment, savedSyncConfig, saveSyncConfig, useSyncLibrary, VaultSession, type SyncConfig } from "./sync";

type View = "recent" | "starred" | "all" | "trash";
type Route = { view: View; noteID?: string };

const viewPaths: Record<View, string> = { recent: "/", starred: "/starred", all: "/all", trash: "/trash" };

function readRoute(pathname = window.location.pathname): Route {
  if (pathname === "/starred") return { view: "starred" };
  if (pathname === "/all") return { view: "all" };
  if (pathname === "/trash") return { view: "trash" };
  const match = pathname.match(/^\/notes\/([^/]+)\/?$/);
  if (match) {
    try { return { view: "recent", noteID: decodeURIComponent(match[1]) }; }
    catch { return { view: "recent" }; }
  }
  return { view: "recent" };
}

function routePath(route: Route) {
  return route.noteID ? `/notes/${encodeURIComponent(route.noteID)}` : viewPaths[route.view];
}

export function App() {
  const initialRoute = useMemo(() => readRoute(), []);
  const [view, setView] = useState<View>(initialRoute.view);
  const [query, setQuery] = useState("");
  const [draft, setDraft] = useState("");
  const [continuedFromID, setContinuedFromID] = useState<string>();
  const [selectedID, setSelectedID] = useState<string | undefined>(initialRoute.noteID);
  const [editing, setEditing] = useState(false);
  const [syncOpen, setSyncOpen] = useState(false);
  const [session, setSession] = useState<VaultSession | null>(null);
  const [configLoaded, setConfigLoaded] = useState(false);
  const [syncStatus, setSyncStatus] = useState("未解锁时不会访问远端。");
  const [syncConfig, setSyncConfig] = useState<SyncConfig>({ relayURL: "", vaultID: "" });
  const [recoveryCode, setRecoveryCode] = useState("");
  const [enrollmentToken, setEnrollmentToken] = useState("");
  const loadedNotes = useLiveQuery(async () => (await db.notes.orderBy("updatedAt").reverse().toArray()), []);
  const notes = loadedNotes ?? [];
  const attachments = useLiveQuery(async () => selectedID ? db.attachments.where("noteId").equals(selectedID).toArray() : [], [selectedID], []);
  const selected = useMemo(() => notes.find(note => note.id === selectedID), [notes, selectedID]);
  const source = useLiveQuery(async () => selectedID ? db.sources.get(selectedID) : undefined, [selectedID]);

  useEffect(() => {
    void migrateLegacyLibrary().then(() => savedSyncConfig()).then(config => { if (config) setSyncConfig(config); setConfigLoaded(true); });
    void navigator.storage?.persist?.();
  }, []);
  useEffect(() => {
    const applyLocation = () => {
      const route = readRoute();
      setView(route.view); setSelectedID(route.noteID); setEditing(false); setSyncOpen(false);
    };
    addEventListener("popstate", applyLocation);
    const canonical = routePath(readRoute());
    if (canonical !== window.location.pathname) history.replaceState(null, "", canonical);
    return () => removeEventListener("popstate", applyLocation);
  }, []);
  useEffect(() => { if (selectedID) window.scrollTo({ top: 0, behavior: "auto" }); }, [selectedID]);
  useEffect(() => { document.title = selected ? `${selected.title} · 拾念` : `${viewTitles[view]} · 拾念`; }, [selected, view]);
  useEffect(() => {
    const handler = () => { if (session && document.visibilityState === "visible") void syncNow(); };
    addEventListener("online", handler); document.addEventListener("visibilitychange", handler);
    const timer = window.setInterval(() => { if (session) void syncNow(); }, 60_000);
    return () => { removeEventListener("online", handler); document.removeEventListener("visibilitychange", handler); clearInterval(timer); };
  }, [session]);
  useEffect(() => {
    if (!session) return;
    let stopped = false;
    const subscribe = async () => {
      while (!stopped) {
        try { await session.waitForChanges(); if (!stopped) await syncNow(); }
        catch { if (!stopped) await new Promise(resolve => setTimeout(resolve, 5_000)); }
      }
    };
    void subscribe();
    return () => { stopped = true; };
  }, [session]);

  const visible = notes.filter(note => {
    if (view === "trash" ? !note.deletedAt : note.deletedAt) return false;
    if (view === "starred" && !note.starred) return false;
    const haystack = `${note.title}\n${note.content}`.toLocaleLowerCase();
    return query.trim().toLocaleLowerCase().split(/\s+/).every(term => haystack.includes(term));
  });

  function navigate(route: Route, replace = false) {
    const path = routePath(route);
    if (replace) history.replaceState(null, "", path); else if (path !== window.location.pathname) history.pushState(null, "", path);
    setView(route.view); setSelectedID(route.noteID); setEditing(false); setSyncOpen(false);
  }
  function showView(nextView: View, replace = false) { navigate({ view: nextView }, replace); }
  function showNote(noteID: string) { navigate({ view, noteID }); }

  async function syncNow() {
    if (!session) return;
    try { const count = await session.sync(); setSyncStatus(count ? `已加密同步 ${count} 条本机变更。` : "已同步，未发现新的本机变更。"); }
    catch (error) { setSyncStatus(error instanceof Error ? error.message : "同步失败"); }
  }
  async function capture() {
    if (!draft.trim()) return;
    const note = await createNote(draft, continuedFromID); setDraft(""); setContinuedFromID(undefined); showNote(note.id); setEditing(true); void syncNow();
  }
  async function update(note: Note, patch: Partial<Note>) {
    const next = { ...note, ...patch, revision: note.revision + 1, updatedAt: now() };
    await saveNote(next); void syncNow();
  }
  async function remove(note: Note) { await update(note, { deletedAt: now() }); showView(view); }
  async function restore(note: Note) { await update(note, { deletedAt: undefined }); }
  async function addImages(files: FileList | File[]) {
    if (!selected) return;
    for (const file of Array.from(files)) {
      if (!file.type.startsWith("image/")) continue;
      const item: Attachment = { id: newID(), syncId: newID(), noteId: selected.id, originalName: file.name || "image", mimeType: file.type, byteSize: file.size, createdAt: now(), blob: file };
      await db.attachments.put(item); await queueAttachment(selected.syncId, item);
    }
    void syncNow();
  }
  function pastedImages(event: ClipboardEvent<HTMLTextAreaElement>) {
    const images = [...event.clipboardData.files].filter(file => file.type.startsWith("image/"));
    if (images.length) { event.preventDefault(); void addImages(images); }
  }
  async function capturePastedImages(event: ClipboardEvent<HTMLTextAreaElement>) {
    const images = [...event.clipboardData.files].filter(file => file.type.startsWith("image/"));
    if (!images.length) return;
    event.preventDefault();
    const note = await createNote(draft || "图片记录", continuedFromID); setDraft(""); setContinuedFromID(undefined); showNote(note.id); setEditing(true);
    for (const file of images) {
      const item: Attachment = { id: newID(), syncId: newID(), noteId: note.id, originalName: file.name || "pasted-image.png", mimeType: file.type, byteSize: file.size, createdAt: now(), blob: file };
      await db.attachments.put(item); await queueAttachment(note.syncId, item);
    }
    void syncNow();
  }
  async function saveSource(url: string, title: string) {
    if (!selected) return;
    const value = { noteId: selected.id, url, title, updatedAt: now() };
    if (url) await db.sources.put(value); else await db.sources.delete(selected.id);
    await queue({ kind: "source.upsert", noteSyncId: selected.syncId, source: value }); void syncNow();
  }
  function download(name: string, contents: string, type: string) {
    const href = URL.createObjectURL(new Blob([contents], { type })); const link = document.createElement("a"); link.href = href; link.download = name; link.click(); URL.revokeObjectURL(href);
  }
  async function downloadBackup() { if (confirm("确认下载完整备份？下载会保存一份当前数据副本。")) download(`thoughtglean-backup-${new Date().toISOString().slice(0, 10)}.json`, JSON.stringify(await exportBackup(), null, 2), "application/json"); }
  async function downloadMarkdown() { if (confirm("确认导出 Markdown？")) download(`thoughtglean-export-${new Date().toISOString().slice(0, 10)}.md`, await markdownExport(), "text/markdown;charset=utf-8"); }
  async function importBackup(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0]; event.target.value = ""; if (!file || !confirm("恢复备份会完全替换当前浏览器内的记录和图片，且无法撤销。继续吗？")) return;
    try { await restoreBackup(JSON.parse(await file.text())); showView("recent"); }
    catch (error) { alert(error instanceof Error ? error.message : "无法恢复备份"); }
  }
  function generatedVault() { const vault = VaultSession.createVault(); setSyncConfig({ relayURL: syncConfig.relayURL, vaultID: vault.vaultID }); setRecoveryCode(pairingCode(vault)); setSyncStatus("已生成配对码。请妥善保存后解锁。"); }
  async function unlock() {
    try { const pairing = parsePairingCode(recoveryCode); const config = { relayURL: syncConfig.relayURL, vaultID: pairing.vaultID }; await saveSyncConfig(config); await useSyncLibrary(config); const unlocked = await VaultSession.unlock({ ...config, recoveryCode: pairing.recoveryCode, enrollmentToken }); await unlocked.claim(); setSession(unlocked); setRecoveryCode(""); setEnrollmentToken(""); setSyncStatus("已解锁，正在同步…"); setSession(unlocked); const count = await unlocked.sync(); setSyncStatus(count ? `已加密同步 ${count} 条本机变更。` : "已解锁并完成同步。"); }
    catch (error) { setSyncStatus(error instanceof Error ? error.message : "无法解锁我的同步库"); }
  }
  function lock() { session?.lock(); setSession(null); setRecoveryCode(""); setSyncStatus("已锁定；不会进行远端读写。"); }

  if (configLoaded && syncConfig.vaultID && !session) return <main className="auth-gate"><section className="auth-gate-card"><span className="auth-gate-mark">⌁</span><p className="eyebrow">ThoughtGlean</p><h1>你的记录已锁定</h1><p>输入恢复代码以解锁本机记录和加密同步。</p><label>恢复代码<input value={recoveryCode} onChange={event => setRecoveryCode(event.target.value)} autoFocus /></label><p className="muted">{syncStatus}</p><button className="button button-primary" onClick={() => void unlock()}>解锁</button><button onClick={() => setSyncOpen(true)}>我的同步设置</button>{syncOpen && <section className="dialog-card"><label>中继地址<input value={syncConfig.relayURL} onChange={event => setSyncConfig({ ...syncConfig, relayURL: event.target.value })} /></label><label>我的同步 ID<input value={syncConfig.vaultID} onChange={event => setSyncConfig({ ...syncConfig, vaultID: event.target.value })} /></label><button onClick={() => setSyncOpen(false)}>关闭</button></section>}</section></main>;

  return <div className="app-shell">
    <header className="topbar"><button className="brand" onClick={() => showView("recent")}><span className="brand-mark" /><span className="brand-copy"><strong>拾念</strong></span></button><label className="search-box"><span className="search-icon" aria-hidden="true" /><input value={query} onChange={event => setQuery(event.target.value)} type="search" placeholder="搜索记录" /><kbd>/</kbd></label><button className="button button-primary top-new" onClick={() => { showView(view === "trash" ? "recent" : view); requestAnimationFrame(() => document.getElementById("captureInput")?.focus()); }}><span>＋</span><span className="button-label">新记录</span></button></header>
    <div className="workspace"><aside className="sidebar"><nav>{([ ["recent", "最近"], ["starred", "星标"], ["all", "全部记录"], ["trash", "回收站"] ] as [View, string][]).map(([key, label]) => <button key={key} className={`nav-item ${view === key ? "active" : ""}`} onClick={() => showView(key)}>{label}</button>)}</nav><nav className="sidebar-secondary"><button className="nav-item" onClick={() => void downloadBackup()}>下载完整备份</button><button className="nav-item" onClick={() => void downloadMarkdown()}>导出 Markdown</button><label className="nav-item">恢复备份<input hidden type="file" accept="application/json,.json" onChange={event => void importBackup(event)} /></label></nav><button className="nav-item" onClick={() => setSyncOpen(true)}>加密同步</button><div className="storage-note"><span className="status-dot" /><span>本机保存<small>数据只在此浏览器中</small></span></div></aside>
      <main className="main-content">{selected ? <Detail note={selected} notes={notes} source={source} attachments={attachments} editing={editing} setEditing={setEditing} onBack={() => showView(view)} onSelect={showNote} onContinue={() => { setContinuedFromID(selected.id); showView(view); }} saveSource={saveSource} update={update} remove={remove} restore={restore} addImages={addImages} pastedImages={pastedImages} /> : selectedID && loadedNotes ? <RouteNotFound onBack={() => showView(view, true)} /> : <Library view={view} notes={visible} draft={draft} continuedFromID={continuedFromID} setDraft={setDraft} clearContinuation={() => setContinuedFromID(undefined)} capture={() => void capture()} paste={event => void capturePastedImages(event)} open={showNote} update={update} restore={restore} />}</main></div>
    {syncOpen && <div className="modal-backdrop" role="presentation"><section className="dialog-card" role="dialog" aria-modal="true"><h2>我的加密同步</h2><p>一个人使用一个同步库。配对码仅保存在你的手中；未解锁时不会访问远端。</p><label>中继地址<input value={syncConfig.relayURL} onChange={event => setSyncConfig({ ...syncConfig, relayURL: event.target.value })} placeholder="https://sync.example.com" /></label><label>恢复配对码<input value={recoveryCode} onChange={event => setRecoveryCode(event.target.value)} placeholder="tg1.…" /></label><label>Relay 注册密钥（仅首次创建）<input value={enrollmentToken} onChange={event => setEnrollmentToken(event.target.value)} /></label><p className="muted">{syncStatus}</p><div className="dialog-actions"><button onClick={generatedVault}>创建我的同步库</button>{session ? <><button onClick={() => void syncNow()}>立即同步</button><button onClick={lock}>锁定</button></> : <button className="button button-primary" onClick={() => void unlock()}>解锁并同步</button>}<button onClick={() => setSyncOpen(false)}>关闭</button></div></section></div>}
  </div>;
}

const viewTitles: Record<View, string> = { recent: "最近", starred: "星标", all: "全部记录", trash: "回收站" };

function RouteNotFound({ onBack }: { onBack: () => void }) {
  return <section className="route-not-found"><p className="eyebrow">404</p><h1>这条记录不存在</h1><p className="muted">它可能已被其他设备永久移除，或者链接不完整。</p><button className="button button-primary" onClick={onBack}>返回记录列表</button></section>;
}

function Library({ view, notes, draft, continuedFromID, setDraft, clearContinuation, capture, paste, open, update, restore }: { view: View; notes: Note[]; draft: string; continuedFromID?: string; setDraft: (value: string) => void; clearContinuation: () => void; capture: () => void; paste: (event: ClipboardEvent<HTMLTextAreaElement>) => void; open: (noteID: string) => void; update: (note: Note, patch: Partial<Note>) => Promise<void>; restore: (note: Note) => Promise<void> }) {
  const groups = new Map<string, Note[]>();
  for (const note of notes) {
    const key = localDateKey(note.createdAt);
    groups.set(key, [...(groups.get(key) ?? []), note]);
  }
  return <section className="library-view">
    <header className="view-heading"><h1>{viewTitles[view]}</h1><p className="muted">{notes.length ? `${notes.length} 条记录` : ""}</p></header>
    {view !== "trash" && <form className="capture-card" onSubmit={event => { event.preventDefault(); capture(); }}>
      {continuedFromID && <div className="continuation-chip"><span>续自记录</span><button type="button" aria-label="取消续写" onClick={clearContinuation}>×</button></div>}
      <textarea id="captureInput" value={draft} onChange={event => setDraft(event.target.value)} onPaste={paste} placeholder={continuedFromID ? "继续写…" : "写下想法…"} />
      <footer className="capture-footer"><span className="draft-state">草稿保存在当前浏览器</span><div className="capture-actions"><span className="shortcut-hint"><kbd>⌘</kbd><kbd>↵</kbd> 保存</span><button className="button button-primary">保存</button></div></footer>
    </form>}
    {notes.length ? <div className="timeline">{[...groups].map(([key, group]) => <DateGroup key={key} dateKey={key} notes={group} view={view} open={open} update={update} restore={restore} />)}</div> : <section className="empty-state"><h2>{view === "trash" ? "回收站是空的" : view === "starred" ? "还没有星标" : "还没有记录"}</h2><p>{view === "trash" ? "删除的记录会保留在这里，可随时恢复。" : "写下第一条内容。"}</p></section>}
  </section>;
}

function DateGroup({ dateKey, notes, view, open, update, restore }: { dateKey: string; notes: Note[]; view: View; open: (noteID: string) => void; update: (note: Note, patch: Partial<Note>) => Promise<void>; restore: (note: Note) => Promise<void> }) {
  const heading = dateHeading(dateKey);
  return <section className="date-group"><h2 className="date-heading">{heading.label}<small>{heading.detail}</small></h2><div className="date-notes">{notes.map(note => <article className="note-row" key={note.id}>
    <button className="note-row-open" onClick={() => open(note.id)}><time className="note-time">{new Date(note.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</time><span className="note-row-main"><strong className="note-row-title">{note.title}</strong><span className="note-excerpt">{note.content}</span>{note.continuedFromId && <span className="note-meta"><span className="relation-label">续写记录</span></span>}</span></button>
    <span className="row-actions">{view === "trash" ? <button className="icon-action restore-action" onClick={() => void restore(note)}>恢复</button> : <button className={`icon-action ${note.starred ? "active" : ""}`} aria-label={note.starred ? "取消星标" : "添加星标"} onClick={() => void update(note, { starred: !note.starred })}>{note.starred ? "★" : "☆"}</button>}</span>
  </article>)}</div></section>;
}

function localDateKey(value: string) {
  const date = new Date(value);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function dateHeading(key: string) {
  const date = new Date(`${key}T00:00:00`);
  const today = new Date();
  const yesterday = new Date(today); yesterday.setDate(today.getDate() - 1);
  const label = key === localDateKey(today.toISOString()) ? "今天" : key === localDateKey(yesterday.toISOString()) ? "昨天" : `${date.getMonth() + 1} 月 ${date.getDate()} 日`;
  return { label, detail: `${date.getFullYear()} 年 · ${["周日", "周一", "周二", "周三", "周四", "周五", "周六"][date.getDay()]}` };
}

function Detail({ note, notes, source, attachments, editing, setEditing, onBack, onSelect, onContinue, saveSource, update, remove, restore, addImages, pastedImages }: { note: Note; notes: Note[]; source?: { url: string; title: string }; attachments: Attachment[]; editing: boolean; setEditing: (value: boolean) => void; onBack: () => void; onSelect: (noteID: string) => void; onContinue: () => void; saveSource: (url: string, title: string) => Promise<void>; update: (note: Note, patch: Partial<Note>) => Promise<void>; remove: (note: Note) => Promise<void>; restore: (note: Note) => Promise<void>; addImages: (files: FileList | File[]) => Promise<void>; pastedImages: (event: ClipboardEvent<HTMLTextAreaElement>) => void }) {
  const [title, setTitle] = useState(note.title); const [content, setContent] = useState(note.content); const [lightbox, setLightbox] = useState<string>();
  const editor = useRef<HTMLTextAreaElement>(null);
  useEffect(() => { setTitle(note.title); setContent(note.content); }, [note]);
  const save = async () => { await update(note, { title: title.trim() || "未命名记录", content }); setEditing(false); };
  const insertCode = () => {
    const element = editor.current; const start = element?.selectionStart ?? content.length; const end = element?.selectionEnd ?? start;
    const block = "```text\n\n```"; const next = `${content.slice(0, start)}${block}${content.slice(end)}`;
    setContent(next); requestAnimationFrame(() => { element?.focus(); element?.setSelectionRange(start + 8, start + 8); });
  };
  const editSource = async () => { const url = prompt("来源链接（留空移除）", source?.url || ""); if (url === null) return; const sourceTitle = url ? prompt("来源标题（可选）", source?.title || "") : ""; if (sourceTitle === null) return; await saveSource(url.trim(), sourceTitle.trim()); };
  const activeNotes = notes.filter(item => !item.deletedAt).sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  const currentIndex = activeNotes.findIndex(item => item.id === note.id);
  const context = activeNotes.slice(Math.max(0, currentIndex - 2), currentIndex + 3);
  return <section className="detail-view"><header className="note-toolbar"><button className="text-button" onClick={onBack}>← 返回</button><div className="note-toolbar-actions"><button className="text-button" onClick={() => void update(note, { starred: !note.starred })}>{note.starred ? "★ 已星标" : "☆ 星标"}</button>{!editing && !note.deletedAt && <button className="text-button" onClick={() => setEditing(true)}>编辑</button>}{note.deletedAt ? <button className="text-button" onClick={() => void restore(note)}>恢复</button> : <button className="text-button danger" onClick={() => void remove(note)}>移到回收站</button>}</div></header>
    <div className="note-detail-layout"><article className="note-paper"><div className="note-origin"><time>{new Date(note.createdAt).toLocaleString()}</time><span>{editing ? "编辑中" : "已保存在本机"}</span></div>
      {editing ? <><input className="note-title-input" value={title} onChange={event => setTitle(event.target.value)} placeholder="标题（可选）" /><textarea ref={editor} className="note-editor" value={content} onChange={event => setContent(event.target.value)} onPaste={pastedImages} /><div className="editor-tools"><button className="button button-secondary" onClick={insertCode}>插入代码片段</button><label className="button button-secondary">添加图片<input hidden type="file" accept="image/*" multiple onChange={event => event.target.files && void addImages(event.target.files)} /></label><button className="button button-secondary" onClick={() => void editSource()}>关联来源</button></div><div className="edit-actions"><button className="button button-ghost" onClick={() => { setTitle(note.title); setContent(note.content); setEditing(false); }}>取消</button><button className="button button-primary" onClick={() => void save()}>保存修改</button></div></> : <><h1 className="note-title">{note.title}</h1><div className="note-rendered"><LimitedMarkdown content={note.content} /></div></>}
      {source && <p className="note-source">来源：<a href={source.url} target="_blank" rel="noreferrer">{source.title || source.url}</a></p>}
      <section className="attachments">{attachments.map(item => <AttachmentPreview key={item.id} item={item} onOpen={setLightbox} onDelete={editing ? async () => { await db.attachments.delete(item.id); await queue({ kind: "attachment.delete", attachmentSyncId: item.syncId }); } : undefined} />)}</section>
    </article><aside className="context-rail"><h2>相关记录</h2><p className="context-copy">这条记录前后的内容。</p><div className="context-timeline">{context.map(item => <div className={`context-item ${item.id === note.id ? "current" : ""}`} key={item.id}><button onClick={() => item.id !== note.id && onSelect(item.id)}><time>{new Date(item.createdAt).toLocaleString()}</time><span>{item.title}</span></button></div>)}</div>{!note.deletedAt && <button className="button button-secondary button-wide" onClick={onContinue}>继续写</button>}</aside></div>
    {lightbox && <div className="image-lightbox" onClick={() => setLightbox(undefined)}><img src={lightbox} alt="查看大图" /></div>}
  </section>;
}

function LimitedMarkdown({ content }: { content: string }) {
  const blocks = content.split(/(^```[^\n]*\n[\s\S]*?^```\s*$)/m).filter(Boolean);
  return <>{blocks.map((block, index) => {
    const match = block.match(/^```([^\n]*)\n([\s\S]*?)^```\s*$/m);
    if (!match) return <p key={index} className="plain-text">{block}</p>;
    const language = match[1].trim() || "text"; const code = match[2];
    return <section className="code-block" key={index}><header><span>{language}</span><button onClick={() => void navigator.clipboard?.writeText(code)}>复制</button></header><pre><code>{code}</code></pre></section>;
  })}</>;
}

function AttachmentPreview({ item, onOpen, onDelete }: { item: Attachment; onOpen: (url: string) => void; onDelete?: () => Promise<void> }) {
  const [url, setURL] = useState("");
  useEffect(() => { const value = URL.createObjectURL(item.blob); setURL(value); return () => URL.revokeObjectURL(value); }, [item.blob]);
  return <span className="attachment-thumb"><button onClick={() => onOpen(url)}><img src={url} alt={item.originalName} /></button>{onDelete && <button aria-label={`移除 ${item.originalName}`} onClick={() => void onDelete()}>×</button>}</span>;
}
