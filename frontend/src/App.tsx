import { useEffect, useMemo, useRef, useState, type ChangeEvent, type ClipboardEvent, type KeyboardEvent, type MouseEvent } from "react";
import { useLiveQuery } from "dexie-react-hooks";
import { createNote, db, deleteLibraryMetadata, exportBackup, markdownExport, migrateLegacyLibrary, newID, now, queue, queueLibrarySnapshot, readLibraryMetadata, restoreBackup, saveNote, writeLibraryMetadata, type Attachment, type Note } from "./db";
import { authStatus, deletePasskey, listPasskeys, login, loginWithPasskey, logout, queueAttachment, registerPasskey, ServerSync, type PasskeyInfo } from "./sync";
import { takeSharedItems, type SharedItem } from "./share";

type View = "recent" | "starred" | "all" | "trash";
type SearchFilter = "all" | "images" | "code" | "source" | "conflicts";
type Route = { view: View; noteID?: string };
type HomeDraft = { content: string; continuedFromID?: string };
type EditDraft = { title: string; content: string };
type ToastState = { message: string; actions?: Array<{ label: string; run: () => void }> };

const homeDraftKey = "draft.home.v1";

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
  const [searchFilter, setSearchFilter] = useState<SearchFilter>("all");
  const [draft, setDraft] = useState("");
  const [continuedFromID, setContinuedFromID] = useState<string>();
  const [draftLoaded, setDraftLoaded] = useState(false);
  const [selectedID, setSelectedID] = useState<string | undefined>(initialRoute.noteID);
  const [editing, setEditing] = useState(false);
  const [authLoaded, setAuthLoaded] = useState(false);
  const [authenticated, setAuthenticated] = useState(false);
  const [accessToken, setAccessToken] = useState("");
  const [loginError, setLoginError] = useState("");
  const [passkeyEnabled, setPasskeyEnabled] = useState(false);
  const [passkeyConfigured, setPasskeyConfigured] = useState(false);
  const [tokenLoginEnabled, setTokenLoginEnabled] = useState(false);
  const [showTokenLogin, setShowTokenLogin] = useState(false);
  const [passkeys, setPasskeys] = useState<PasskeyInfo[]>([]);
  const [passkeyBusy, setPasskeyBusy] = useState(false);
  const [passkeyMessage, setPasskeyMessage] = useState("");
  const [syncStatus, setSyncStatus] = useState("本机保存；联网后自动同步。");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [toast, setToast] = useState<ToastState>();
  const syncClient = useMemo(() => new ServerSync(), []);
  const backupInput = useRef<HTMLInputElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const shareInboxChecked = useRef(false);
  const loadedNotes = useLiveQuery(async () => (await db.notes.orderBy("updatedAt").reverse().toArray()), []);
  const notes = loadedNotes ?? [];
  const allAttachments = useLiveQuery(() => db.attachments.toArray(), [], []);
  const allSources = useLiveQuery(() => db.sources.toArray(), [], []);
  const attachments = useLiveQuery(async () => selectedID ? db.attachments.where("noteId").equals(selectedID).toArray() : [], [selectedID], []);
  const selected = useMemo(() => notes.find(note => note.id === selectedID), [notes, selectedID]);
  const source = useLiveQuery(async () => selectedID ? db.sources.get(selectedID) : undefined, [selectedID]);

  useEffect(() => {
    void migrateLegacyLibrary().then(async () => {
      const savedDraft = await readLibraryMetadata<HomeDraft>(homeDraftKey);
      if (savedDraft) { setDraft(savedDraft.content); setContinuedFromID(savedDraft.continuedFromID); }
      setDraftLoaded(true);
      try {
        const status = await authStatus();
        setAuthenticated(status.authenticated);
        setPasskeyEnabled(status.enabled);
        setPasskeyConfigured(status.configured);
        setTokenLoginEnabled(status.tokenLoginEnabled);
        setShowTokenLogin(!status.configured);
        if (status.authenticated) localStorage.setItem("thoughtglean.owner-authenticated", "1");
      } catch {
        if (localStorage.getItem("thoughtglean.owner-authenticated") === "1") {
          setAuthenticated(true);
          setSyncStatus("离线使用；连接服务器后将自动同步。");
        }
      } finally { setAuthLoaded(true); }
    });
    void navigator.storage?.persist?.();
  }, []);
  useEffect(() => {
    if (!draftLoaded) return;
    const timer = window.setTimeout(() => {
      if (draft || continuedFromID) void writeLibraryMetadata(homeDraftKey, { content: draft, continuedFromID } satisfies HomeDraft);
      else void deleteLibraryMetadata(homeDraftKey);
    }, 250);
    return () => clearTimeout(timer);
  }, [draft, continuedFromID, draftLoaded]);
  useEffect(() => {
    const shortcut = (event: globalThis.KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      if (event.key === "/" && !event.metaKey && !event.ctrlKey && !event.altKey && !target?.closest("input, textarea, [contenteditable=true]")) {
        event.preventDefault(); searchInput.current?.focus();
      }
    };
    addEventListener("keydown", shortcut);
    return () => removeEventListener("keydown", shortcut);
  }, []);
  useEffect(() => {
    if (!toast) return;
    const timer = window.setTimeout(() => setToast(undefined), 5000);
    return () => clearTimeout(timer);
  }, [toast]);
  useEffect(() => {
    const applyLocation = () => {
      const route = readRoute();
      setView(route.view); setSelectedID(route.noteID); setEditing(false);
    };
    addEventListener("popstate", applyLocation);
    const canonical = routePath(readRoute());
    if (canonical !== window.location.pathname) history.replaceState(null, "", canonical);
    return () => removeEventListener("popstate", applyLocation);
  }, []);
  useEffect(() => { if (selectedID) window.scrollTo({ top: 0, behavior: "auto" }); }, [selectedID]);
  useEffect(() => { document.title = selected ? `${selected.title} · 拾念` : `${viewTitles[view]} · 拾念`; }, [selected, view]);
  useEffect(() => {
    const handler = () => { if (authenticated && document.visibilityState === "visible") void syncNow(); };
    addEventListener("online", handler); document.addEventListener("visibilitychange", handler);
    const timer = window.setInterval(() => { if (authenticated) void syncNow(); }, 60_000);
    return () => { removeEventListener("online", handler); document.removeEventListener("visibilitychange", handler); clearInterval(timer); };
  }, [authenticated]);
  useEffect(() => { if (authenticated) void syncNow(); }, [authenticated]);
  useEffect(() => {
    if (!authenticated || shareInboxChecked.current) return;
    shareInboxChecked.current = true;
    void takeSharedItems().then(importSharedItems).catch(() => { shareInboxChecked.current = false; });
  }, [authenticated]);
  useEffect(() => {
    if (!settingsOpen || !passkeyConfigured) return;
    void listPasskeys().then(setPasskeys).catch(error => setPasskeyMessage(error instanceof Error ? error.message : "无法读取 Passkey"));
  }, [settingsOpen, passkeyConfigured]);

  const attachmentNoteIDs = new Set(allAttachments.map(item => item.noteId));
  const sourceNoteIDs = new Set(allSources.map(item => item.noteId));
  const conflictNotes = notes.filter(note => !note.deletedAt && isConflict(note));
  const visible = notes.filter(note => {
    if (view === "trash" ? !note.deletedAt : note.deletedAt) return false;
    if (view === "starred" && !note.starred) return false;
    if (searchFilter === "images" && !attachmentNoteIDs.has(note.id)) return false;
    if (searchFilter === "code" && !note.content.includes("```")) return false;
    if (searchFilter === "source" && !sourceNoteIDs.has(note.id)) return false;
    if (searchFilter === "conflicts" && !isConflict(note)) return false;
    const haystack = `${note.title}\n${note.content}`.toLocaleLowerCase();
    return query.trim().toLocaleLowerCase().split(/\s+/).every(term => haystack.includes(term));
  });

  function navigate(route: Route, replace = false) {
    const path = routePath(route);
    if (replace) history.replaceState(null, "", path); else if (path !== window.location.pathname) history.pushState(null, "", path);
    setView(route.view); setSelectedID(route.noteID); setEditing(false);
  }
  function showView(nextView: View, replace = false) { navigate({ view: nextView }, replace); }
  function showNote(noteID: string) { navigate({ view, noteID }); }

  async function syncNow() {
    if (!authenticated || !navigator.onLine) return;
    try { const count = await syncClient.sync(); setSyncStatus(count ? `已同步 ${count} 条本机变更。` : `已同步 · ${new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`); }
    catch (error) {
      const message = error instanceof Error ? error.message : "同步失败";
      setSyncStatus(message);
      if (message === "需要登录") { setAuthenticated(false); localStorage.removeItem("thoughtglean.owner-authenticated"); }
    }
  }
  async function capture() {
    if (!draft.trim()) return;
    const note = await createNote(draft, continuedFromID);
    setDraft(""); setContinuedFromID(undefined); await deleteLibraryMetadata(homeDraftKey);
    setToast({ message: "记录已保存", actions: [
      { label: "打开", run: () => showNote(note.id) },
      { label: "撤销", run: () => { void update(note, { deletedAt: now() }); setToast({ message: "已移到回收站" }); } },
    ] });
    requestAnimationFrame(() => document.getElementById("captureInput")?.focus());
    void syncNow();
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
  async function importSharedItems(items: SharedItem[]) {
    if (!items.length) return;
    let imageNote: Note | undefined;
    for (const shared of items) {
      const parts = [shared.title.trim(), shared.text.trim(), shared.url.trim()].filter((part, index, values) => part && values.indexOf(part) === index);
      const note = await createNote(parts.join("\n\n") || "图片记录");
      if (shared.images.length) {
        imageNote = note;
        for (const file of shared.images) {
          const item: Attachment = { id: newID(), syncId: newID(), noteId: note.id, originalName: file.name || "shared-image", mimeType: file.type, byteSize: file.size, createdAt: now(), blob: file };
          await db.attachments.put(item); await queueAttachment(note.syncId, item);
        }
      }
    }
    if (imageNote) { showNote(imageNote.id); setEditing(true); }
    else setToast({ message: items.length === 1 ? "分享内容已保存" : `已保存 ${items.length} 条分享内容` });
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
    try { await restoreBackup(JSON.parse(await file.text())); await queueLibrarySnapshot(); showView("recent"); void syncNow(); }
    catch (error) { alert(error instanceof Error ? error.message : "无法恢复备份"); }
  }
  async function signIn() {
    if (!accessToken) return;
    try {
      setLoginError(""); await login(accessToken); setAccessToken(""); setAuthenticated(true);
      localStorage.setItem("thoughtglean.owner-authenticated", "1");
    } catch (error) { setLoginError(error instanceof Error ? error.message : "登录失败"); }
  }
  async function signInWithPasskey() {
    try {
      setLoginError(""); setPasskeyBusy(true); await loginWithPasskey(); setAuthenticated(true);
      localStorage.setItem("thoughtglean.owner-authenticated", "1");
    } catch (error) { setLoginError(error instanceof Error ? error.message : "Passkey 登录失败"); }
    finally { setPasskeyBusy(false); }
  }
  async function setupPasskey() {
    try {
      setPasskeyMessage(""); setPasskeyBusy(true); await registerPasskey(passkeyConfigured);
      setPasskeyConfigured(true); setPasskeys(await listPasskeys());
      setPasskeyMessage(passkeyConfigured ? "备用 Passkey 已添加。" : "Passkey 已设置，今后可直接用设备验证登录。");
    } catch (error) { setPasskeyMessage(error instanceof Error ? error.message : "Passkey 设置失败"); }
    finally { setPasskeyBusy(false); }
  }
  async function removePasskey(item: PasskeyInfo) {
    if (passkeys.length <= 1 || !confirm("删除这个 Passkey？删除后，这台设备保存的凭据将不能再用于登录。")) return;
    try { setPasskeyMessage(""); await deletePasskey(item.id); setPasskeys(await listPasskeys()); setPasskeyMessage("Passkey 已删除。"); }
    catch (error) { setPasskeyMessage(error instanceof Error ? error.message : "无法删除 Passkey"); }
  }
  async function signOut() {
    try { await logout(); } finally {
      localStorage.removeItem("thoughtglean.owner-authenticated"); setAuthenticated(false); setSettingsOpen(false); setSyncStatus("已退出登录。");
    }
  }

  if (!authLoaded) return <main className="auth-gate"><p className="muted">正在连接服务器…</p></main>;
  if (!authenticated) return <main className="auth-gate"><form className="auth-gate-card" onSubmit={event => { event.preventDefault(); if (showTokenLogin) void signIn(); else void signInWithPasskey(); }}>
    <header className="auth-gate-header"><span className="auth-gate-brand"><i />拾念</span><p>THOUGHTGLEAN</p><h1>{showTokenLogin || !passkeyConfigured ? "使用密钥登录" : "欢迎回来"}</h1><span className="auth-gate-description">{passkeyEnabled && passkeyConfigured && !showTokenLogin ? "使用设备上的 Passkey 快速登录。" : passkeyEnabled && !passkeyConfigured ? "登录后可在设置中启用 Passkey。" : "输入服务器配置的个人访问密钥。"}</span></header>
    <div className="auth-gate-fields">{passkeyEnabled && passkeyConfigured && !showTokenLogin ? <>
      <button className="button button-primary button-wide auth-submit" type="submit" disabled={passkeyBusy}>{passkeyBusy ? "正在验证…" : "使用 Passkey 登录"}</button>
      {tokenLoginEnabled && <button className="text-button auth-alternative" type="button" onClick={() => { setShowTokenLogin(true); setLoginError(""); }}>改用个人访问密钥</button>}
    </> : <>
      <label htmlFor="ownerToken">个人访问密钥</label><input id="ownerToken" type="password" value={accessToken} onChange={event => setAccessToken(event.target.value)} autoFocus autoComplete="current-password" placeholder="输入访问密钥" />
      <button className="button button-primary button-wide auth-submit" type="submit" disabled={!accessToken}>登录</button>
      {passkeyConfigured && <button className="text-button auth-alternative" type="button" onClick={() => { setShowTokenLogin(false); setLoginError(""); }}>返回 Passkey 登录</button>}
    </>}{loginError && <p className="inline-error" role="alert">{loginError}</p>}</div>
    <footer className="auth-gate-footer">个人应用 · 数据仅向你的服务器同步</footer>
  </form></main>;

  return <div className="app-shell">
    <header className="topbar"><button className="brand" onClick={() => showView("recent")}><span className="brand-mark" /><span className="brand-copy"><strong>拾念</strong></span></button><label className="search-box"><span className="search-icon" aria-hidden="true" /><input ref={searchInput} value={query} onChange={event => setQuery(event.target.value)} type="search" placeholder="搜索记录" /><kbd>/</kbd></label><button className="button button-primary top-new" onClick={() => { showView(view === "trash" ? "recent" : view); requestAnimationFrame(() => document.getElementById("captureInput")?.focus()); }}><span>＋</span><span className="button-label">新记录</span></button></header>
    <div className="workspace"><aside className="sidebar"><nav className="primary-nav">{([ ["recent", "最近"], ["starred", "星标"], ["all", "全部"], ["trash", "回收站"] ] as [View, string][]).map(([key, label]) => <button key={key} className={`nav-item ${view === key ? "active" : ""}`} onClick={() => showView(key)}>{label}</button>)}</nav><button className="nav-item settings-nav" onClick={() => setSettingsOpen(true)}>设置</button><div className="storage-note"><span className="status-dot" /><span>{syncStatus}<small>本机离线副本 · 服务端同步</small></span></div></aside>
      <main className="main-content">{selected ? <Detail note={selected} notes={notes} source={source} attachments={attachments} editing={editing} setEditing={setEditing} onBack={() => showView(view)} onSelect={showNote} onContinue={() => { setContinuedFromID(selected.id); showView(view); }} saveSource={saveSource} update={update} remove={remove} restore={restore} addImages={addImages} pastedImages={pastedImages} /> : selectedID && loadedNotes ? <RouteNotFound onBack={() => showView(view, true)} /> : <Library view={view} notes={visible} query={query} searchFilter={searchFilter} setSearchFilter={setSearchFilter} conflictCount={conflictNotes.length} openFirstConflict={() => conflictNotes[0] && showNote(conflictNotes[0].id)} draft={draft} continuedFromID={continuedFromID} setDraft={setDraft} clearContinuation={() => setContinuedFromID(undefined)} capture={() => void capture()} paste={event => void capturePastedImages(event)} open={showNote} update={update} restore={restore} />}</main></div>
    {settingsOpen && <div className="modal-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget) setSettingsOpen(false); }}><section className="dialog-card settings-dialog" role="dialog" aria-modal="true" aria-label="设置"><header><div><p className="eyebrow">ThoughtGlean</p><h2>设置</h2></div><button className="text-button" onClick={() => setSettingsOpen(false)}>关闭</button></header><div className="settings-section"><h3>数据</h3><button className="button button-secondary" onClick={() => void downloadBackup()}>下载完整备份</button><button className="button button-secondary" onClick={() => void downloadMarkdown()}>导出 Markdown</button><button className="button button-secondary" onClick={() => backupInput.current?.click()}>恢复备份</button><input ref={backupInput} hidden type="file" accept="application/json,.json" onChange={event => void importBackup(event)} /></div><div className="settings-section"><h3>同步</h3><p className="muted">{syncStatus}</p><button className="button button-secondary" onClick={() => void syncNow()}>立即同步</button></div>{passkeyEnabled && <div className="settings-section passkey-section"><h3>Passkey</h3><p className="muted">{passkeyConfigured ? "使用指纹、面容或设备解锁登录，无需重复输入访问密钥。" : "为这台设备设置快速、安全的登录方式。"}</p>{passkeys.map((item, index) => <div className="passkey-row" key={item.id}><span><strong>Passkey {index + 1}</strong><small>添加于 {new Date(item.createdAt).toLocaleDateString()}</small></span>{passkeys.length > 1 && <button className="text-button danger" onClick={() => void removePasskey(item)}>删除</button>}</div>)}<button className="button button-secondary" disabled={passkeyBusy} onClick={() => void setupPasskey()}>{passkeyBusy ? "等待设备验证…" : passkeyConfigured ? "添加备用 Passkey" : "设置 Passkey"}</button>{passkeyMessage && <p className={passkeyMessage.includes("失败") || passkeyMessage.includes("无法") ? "inline-error" : "inline-success"}>{passkeyMessage}</p>}</div>}<div className="settings-section settings-danger"><button className="button button-secondary" onClick={() => void signOut()}>退出登录</button></div></section></div>}
    {toast && <aside className="toast" role="status"><span>{toast.message}</span>{toast.actions?.map(action => <button key={action.label} onClick={() => { action.run(); setToast(undefined); }}>{action.label}</button>)}</aside>}
  </div>;
}

const viewTitles: Record<View, string> = { recent: "最近", starred: "星标", all: "全部记录", trash: "回收站" };
const isConflict = (note: Note) => note.title.startsWith("同步冲突：") || note.title === "同步冲突记录";

function HighlightedText({ text, query }: { text: string; query: string }) {
  const terms = query.trim().split(/\s+/).filter(Boolean);
  if (!terms.length) return text;
  const escaped = terms.map(term => term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const matcher = new RegExp(`(${escaped.join("|")})`, "gi");
  return <>{text.split(matcher).map((part, index) => terms.some(term => part.toLocaleLowerCase() === term.toLocaleLowerCase()) ? <mark key={index}>{part}</mark> : part)}</>;
}

function RouteNotFound({ onBack }: { onBack: () => void }) {
  return <section className="route-not-found"><p className="eyebrow">404</p><h1>这条记录不存在</h1><p className="muted">它可能已被其他设备永久移除，或者链接不完整。</p><button className="button button-primary" onClick={onBack}>返回记录列表</button></section>;
}

function Library({ view, notes, query, searchFilter, setSearchFilter, conflictCount, openFirstConflict, draft, continuedFromID, setDraft, clearContinuation, capture, paste, open, update, restore }: { view: View; notes: Note[]; query: string; searchFilter: SearchFilter; setSearchFilter: (value: SearchFilter) => void; conflictCount: number; openFirstConflict: () => void; draft: string; continuedFromID?: string; setDraft: (value: string) => void; clearContinuation: () => void; capture: () => void; paste: (event: ClipboardEvent<HTMLTextAreaElement>) => void; open: (noteID: string) => void; update: (note: Note, patch: Partial<Note>) => Promise<void>; restore: (note: Note) => Promise<void> }) {
  const groups = new Map<string, Note[]>();
  for (const note of notes) {
    const key = localDateKey(note.createdAt);
    groups.set(key, [...(groups.get(key) ?? []), note]);
  }
  return <section className="library-view">
    <header className="view-heading"><h1>{viewTitles[view]}</h1><p className="muted">{notes.length ? `${notes.length} 条记录` : ""}</p></header>
    <div className="search-filters" aria-label="搜索筛选">{([['all', '全部'], ['images', '图片'], ['code', '代码'], ['source', '来源'], ['conflicts', '冲突']] as [SearchFilter, string][]).map(([key, label]) => <button key={key} aria-pressed={searchFilter === key} onClick={() => setSearchFilter(key)}>{label}</button>)}</div>
    {conflictCount > 0 && searchFilter !== "conflicts" && view !== "trash" && <aside className="conflict-notice"><span><strong>{conflictCount} 条同步冲突待处理</strong><small>原版本和另一设备版本均已保留。</small></span><button className="button button-secondary" onClick={openFirstConflict}>查看</button></aside>}
    {view !== "trash" && <form className="capture-card" onSubmit={event => { event.preventDefault(); capture(); }}>
      {continuedFromID && <div className="continuation-chip"><span>续自记录</span><button type="button" aria-label="取消续写" onClick={clearContinuation}>×</button></div>}
      <textarea id="captureInput" value={draft} onChange={event => setDraft(event.target.value)} onPaste={paste} onKeyDown={event => { if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); capture(); } }} placeholder={continuedFromID ? "继续写…" : "写下想法…"} />
      <footer className="capture-footer"><span className="draft-state">草稿自动保存 · 首行 <code># 标题</code> 可设置标题</span><div className="capture-actions"><span className="shortcut-hint"><kbd>⌘</kbd><kbd>↵</kbd> 保存</span><button className="button button-primary">保存</button></div></footer>
    </form>}
    {notes.length ? <div className="timeline">{[...groups].map(([key, group]) => <DateGroup key={key} dateKey={key} notes={group} query={query} view={view} open={open} update={update} restore={restore} />)}</div> : <section className="empty-state"><h2>{view === "trash" ? "回收站是空的" : view === "starred" ? "还没有星标" : query || searchFilter !== "all" ? "没有匹配的记录" : "还没有记录"}</h2><p>{view === "trash" ? "删除的记录会保留在这里，可随时恢复。" : query || searchFilter !== "all" ? "换个关键词或筛选条件试试。" : "写下第一条内容。"}</p></section>}
  </section>;
}

function DateGroup({ dateKey, notes, query, view, open, update, restore }: { dateKey: string; notes: Note[]; query: string; view: View; open: (noteID: string) => void; update: (note: Note, patch: Partial<Note>) => Promise<void>; restore: (note: Note) => Promise<void> }) {
  const heading = dateHeading(dateKey);
  return <section className="date-group"><h2 className="date-heading">{heading.label}<small>{heading.detail}</small></h2><div className="date-notes">{notes.map(note => <article className="note-row" key={note.id}>
    <button className="note-row-open" onClick={() => open(note.id)}><time className="note-time">{new Date(note.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</time><span className="note-row-main"><strong className="note-row-title"><HighlightedText text={note.title} query={query} /></strong><span className="note-excerpt"><HighlightedText text={note.content} query={query} /></span>{note.continuedFromId && <span className="note-meta"><span className="relation-label">{isConflict(note) ? "同步冲突" : "续写记录"}</span></span>}</span></button>
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
  const [editDraftLoaded, setEditDraftLoaded] = useState(false);
  const [hasEditDraft, setHasEditDraft] = useState(false);
  const titleEditor = useRef<HTMLInputElement>(null);
  const editor = useRef<HTMLTextAreaElement>(null);
  const editFocus = useRef<"title" | "body">("body");
  const editDraftKey = `draft.note.${note.syncId}`;
  useEffect(() => {
    let active = true;
    setEditDraftLoaded(false);
    void readLibraryMetadata<EditDraft>(editDraftKey).then(saved => {
      if (!active) return;
      setTitle(saved?.title ?? note.title); setContent(saved?.content ?? note.content);
      setHasEditDraft(Boolean(saved)); setEditDraftLoaded(true);
    });
    return () => { active = false; };
  }, [note.id]);
  useEffect(() => {
    if (!editing && editDraftLoaded && !hasEditDraft) { setTitle(note.title); setContent(note.content); }
  }, [note.updatedAt, editing, editDraftLoaded, hasEditDraft]);
  useEffect(() => {
    if (!editing || !editDraftLoaded) return;
    const timer = window.setTimeout(() => {
      if (title !== note.title || content !== note.content) {
        setHasEditDraft(true); void writeLibraryMetadata(editDraftKey, { title, content } satisfies EditDraft);
      } else {
        setHasEditDraft(false); void deleteLibraryMetadata(editDraftKey);
      }
    }, 250);
    return () => clearTimeout(timer);
  }, [title, content, editing, editDraftLoaded, editDraftKey, note.title, note.content]);
  useEffect(() => { if (editing) requestAnimationFrame(() => (editFocus.current === "title" ? titleEditor.current : editor.current)?.focus()); }, [editing]);
  const save = async () => { await update(note, { title: title.trim() || "未命名记录", content }); await deleteLibraryMetadata(editDraftKey); setHasEditDraft(false); setEditing(false); };
  const cancel = () => { setTitle(note.title); setContent(note.content); setHasEditDraft(false); void deleteLibraryMetadata(editDraftKey); setEditing(false); };
  const editorKeyDown = (event: KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    if (event.key === "Escape") { event.preventDefault(); cancel(); }
    else if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); void save(); }
  };
  const beginEditing = (focus: "title" | "body" = "body") => { if (!note.deletedAt) { editFocus.current = focus; setEditing(true); } };
  const activateBodyEditor = (event: MouseEvent<HTMLDivElement>) => {
    const selection = window.getSelection();
    if ((event.target as Element).closest("a, button, pre, code, img") || (selection && !selection.isCollapsed)) return;
    beginEditing();
  };
  const insertCode = () => {
    const element = editor.current; const start = element?.selectionStart ?? content.length; const end = element?.selectionEnd ?? start;
    const block = "```text\n\n```"; const next = `${content.slice(0, start)}${block}${content.slice(end)}`;
    setContent(next); requestAnimationFrame(() => { element?.focus(); element?.setSelectionRange(start + 8, start + 8); });
  };
  const editSource = async () => { const url = prompt("来源链接（留空移除）", source?.url || ""); if (url === null) return; const sourceTitle = url ? prompt("来源标题（可选）", source?.title || "") : ""; if (sourceTitle === null) return; await saveSource(url.trim(), sourceTitle.trim()); };
  const activeNotes = notes.filter(item => !item.deletedAt).sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  const currentIndex = activeNotes.findIndex(item => item.id === note.id);
  const context = activeNotes.slice(Math.max(0, currentIndex - 2), currentIndex + 3);
  return <section className="detail-view"><header className="note-toolbar"><button className="text-button" onClick={onBack}>← 返回</button><div className="note-toolbar-actions"><button className="text-button" onClick={() => void update(note, { starred: !note.starred })}>{note.starred ? "★ 已星标" : "☆ 星标"}</button>{note.deletedAt ? <button className="text-button" onClick={() => void restore(note)}>恢复</button> : <button className="text-button danger" onClick={() => void remove(note)}>移到回收站</button>}</div></header>
    <div className="note-detail-layout"><article className="note-paper">{isConflict(note) && <aside className="conflict-detail"><span><strong>这是另一设备保留下来的冲突版本</strong><small>比较相关记录中的原版本，合并需要的内容后删除或重命名本记录即可完成处理。</small></span>{!editing && !note.deletedAt && <button className="button button-secondary" onClick={() => beginEditing("title")}>开始处理</button>}</aside>}<div className="note-origin"><time>{new Date(note.createdAt).toLocaleString()}</time><span>{editing ? "编辑中" : "已保存在本机"}</span></div>
      {editing ? <><input ref={titleEditor} className="note-title-input" value={title} onChange={event => setTitle(event.target.value)} onKeyDown={editorKeyDown} placeholder="标题（可选）" /><textarea ref={editor} className="note-editor" value={content} onChange={event => setContent(event.target.value)} onPaste={pastedImages} onKeyDown={editorKeyDown} /><div className="editor-tools"><button className="button button-secondary" onClick={insertCode}>插入代码片段</button><label className="button button-secondary">添加图片<input hidden type="file" accept="image/*" multiple onChange={event => event.target.files && void addImages(event.target.files)} /></label><button className="button button-secondary" onClick={() => void editSource()}>关联来源</button></div><div className="edit-actions"><span className="shortcut-hint"><kbd>Esc</kbd> 取消 · <kbd>⌘</kbd><kbd>↵</kbd> 保存</span><button className="button button-ghost" onClick={cancel}>取消</button><button className="button button-primary" onClick={() => void save()}>保存修改</button></div></> : <><div className="note-edit-surface note-title-surface" role="button" tabIndex={note.deletedAt ? -1 : 0} aria-label="编辑标题和正文" onClick={() => beginEditing("title")} onKeyDown={event => { if (event.key === "Enter") beginEditing("title"); }}><h1 className="note-title">{note.title}</h1></div><div className="note-edit-surface note-rendered" role="button" tabIndex={note.deletedAt ? -1 : 0} aria-label="编辑正文" onClick={activateBodyEditor} onKeyDown={event => { if (event.key === "Enter") beginEditing(); }}><LimitedMarkdown content={note.content} /></div></>}
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
