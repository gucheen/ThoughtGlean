import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ChangeEvent, type ClipboardEvent, type KeyboardEvent, type MouseEvent, type RefObject } from "react";
import { useLiveQuery } from "dexie-react-hooks";
import { createNote, db, deleteAttachment, deleteLibraryMetadata, exportBackup, markdownExport, migrateLegacyLibrary, newID, now, queue, queueLibrarySnapshot, readLibraryMetadata, restoreBackup, saveAttachment, saveNote, updateAttachmentAlt, writeLibraryMetadata, type Attachment, type Note } from "./db";
import { authStatus, deletePasskey, listPasskeys, login, loginWithPasskey, logout, registerPasskey, serverInfo, ServerSync, type PasskeyInfo } from "./sync";
import { takeSharedItems, type SharedItem } from "./share";
import { applyPWAUpdate, pwaUpdateEvent } from "./pwa";

type View = "recent" | "starred" | "all" | "trash";
type SearchFilter = "all" | "images" | "code" | "source" | "conflicts";
type TimeFilter = "any" | "today" | "week" | "month";
type Route = { view: View; noteID?: string };
type HomeDraft = { content: string; continuedFromID?: string; sourceURL?: string };
type EditDraft = { title: string; content: string };
type ToastState = { message: string; persistent?: boolean; actions?: Array<{ label: string; run: () => void }> };
type SyncErrorInfo = { message: string; at: string };

const homeDraftKey = "draft.home.v1";
const recentSearchesKey = "search.recent.v1";
const lastSyncSuccessKey = "sync.last-success.v1";
const lastSyncErrorKey = "sync.last-error.v1";
const captureMaxHeight = 320;

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
  const [timeFilter, setTimeFilter] = useState<TimeFilter>("any");
  const [recentSearches, setRecentSearches] = useState<string[]>([]);
  const [searchSelection, setSearchSelection] = useState(0);
  const [draft, setDraft] = useState("");
  const [captureBusy, setCaptureBusy] = useState(false);
  const [draftStatus, setDraftStatus] = useState<"idle" | "saving" | "saved">("idle");
  const [suggestedSource, setSuggestedSource] = useState<string>();
  const [captureSource, setCaptureSource] = useState<string>();
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
  const [lastSyncSuccess, setLastSyncSuccess] = useState<string>();
  const [lastSyncError, setLastSyncError] = useState<SyncErrorInfo>();
  const [serverVersion, setServerVersion] = useState("读取中…");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [toast, setToast] = useState<ToastState>();
  const syncClient = useMemo(() => new ServerSync(), []);
  const backupInput = useRef<HTMLInputElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const captureInput = useRef<HTMLTextAreaElement>(null);
  const draftSaveVersion = useRef(0);
  const shareInboxChecked = useRef(false);
  const loadedNotes = useLiveQuery(async () => (await db.notes.orderBy("updatedAt").reverse().toArray()), []);
  const notes = loadedNotes ?? [];
  const pendingSyncCount = useLiveQuery(() => db.events.count(), [], 0);
  const allAttachments = useLiveQuery(() => db.attachments.toArray(), [], []);
  const allSources = useLiveQuery(() => db.sources.toArray(), [], []);
  const attachments = useLiveQuery(async () => selectedID ? db.attachments.where("noteId").equals(selectedID).toArray() : [], [selectedID], []);
  const selected = useMemo(() => notes.find(note => note.id === selectedID), [notes, selectedID]);
  const source = useLiveQuery(async () => selectedID ? db.sources.get(selectedID) : undefined, [selectedID]);

  useEffect(() => {
    void migrateLegacyLibrary().then(async () => {
      const savedDraft = await readLibraryMetadata<HomeDraft>(homeDraftKey);
      const savedSearches = await readLibraryMetadata<string[]>(recentSearchesKey);
      const savedSyncSuccess = await readLibraryMetadata<string>(lastSyncSuccessKey);
      const savedSyncError = await readLibraryMetadata<SyncErrorInfo>(lastSyncErrorKey);
      if (savedDraft) { setDraft(savedDraft.content); setContinuedFromID(savedDraft.continuedFromID); setCaptureSource(savedDraft.sourceURL); }
      if (Array.isArray(savedSearches)) setRecentSearches(savedSearches.filter(item => typeof item === "string").slice(0, 6));
      if (typeof savedSyncSuccess === "string") setLastSyncSuccess(savedSyncSuccess);
      if (savedSyncError?.message && savedSyncError.at) setLastSyncError(savedSyncError);
      setDraftLoaded(true); setDraftStatus("saved");
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
    const version = ++draftSaveVersion.current;
    setDraftStatus("saving");
    const timer = window.setTimeout(async () => {
      if (draft || continuedFromID || captureSource) await writeLibraryMetadata(homeDraftKey, { content: draft, continuedFromID, sourceURL: captureSource } satisfies HomeDraft);
      else await deleteLibraryMetadata(homeDraftKey);
      if (draftSaveVersion.current === version) setDraftStatus("saved");
    }, 250);
    return () => clearTimeout(timer);
  }, [draft, continuedFromID, captureSource, draftLoaded]);
  useEffect(() => {
    setSearchSelection(0);
    const value = query.trim();
    if (value.length < 2) return;
    const timer = window.setTimeout(() => {
      setRecentSearches(current => {
        const next = [value, ...current.filter(item => normalizeForSearch(item) !== normalizeForSearch(value))].slice(0, 6);
        void writeLibraryMetadata(recentSearchesKey, next);
        return next;
      });
    }, 900);
    return () => clearTimeout(timer);
  }, [query]);
  useEffect(() => {
    const shortcut = (event: globalThis.KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      if (event.key === "/" && !event.metaKey && !event.ctrlKey && !event.altKey && !target?.closest("input, textarea, [contenteditable=true]")) {
        event.preventDefault(); searchInput.current?.focus();
      } else if (event.key.toLocaleLowerCase() === "n" && !event.metaKey && !event.ctrlKey && !event.altKey && !target?.closest("input, textarea, [contenteditable=true]")) {
        event.preventDefault(); document.querySelector<HTMLButtonElement>(".top-new")?.click();
      }
    };
    addEventListener("keydown", shortcut);
    return () => removeEventListener("keydown", shortcut);
  }, []);
  useEffect(() => {
    if (!toast || toast.persistent) return;
    const timer = window.setTimeout(() => setToast(undefined), 5000);
    return () => clearTimeout(timer);
  }, [toast]);
  useEffect(() => {
    const available = () => setToast({ message: "新版本已准备好", persistent: true, actions: [
      { label: "立即更新", run: applyPWAUpdate }, { label: "稍后", run: () => undefined },
    ] });
    addEventListener(pwaUpdateEvent, available);
    return () => removeEventListener(pwaUpdateEvent, available);
  }, []);
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
  useEffect(() => {
    if (!settingsOpen) return;
    setServerVersion("读取中…");
    void serverInfo().then(info => setServerVersion(info.version || "未知")).catch(() => setServerVersion("无法连接"));
  }, [settingsOpen]);

  const attachmentNoteIDs = new Set(allAttachments.map(item => item.noteId));
  const sourceNoteIDs = new Set(allSources.map(item => item.noteId));
  const sourceByNote = new Map(allSources.map(item => [item.noteId, item]));
  const attachmentNamesByNote = new Map<string, string[]>();
  for (const item of allAttachments) attachmentNamesByNote.set(item.noteId, [...(attachmentNamesByNote.get(item.noteId) ?? []), item.originalName, ...(item.altText ? [item.altText] : [])]);
  const terms = searchTerms(query);
  const conflictNotes = notes.filter(note => !note.deletedAt && isConflict(note));
  const visible = notes.filter(note => {
    if (view === "trash" ? !note.deletedAt : note.deletedAt) return false;
    if (view === "starred" && !note.starred) return false;
    if (searchFilter === "images" && !attachmentNoteIDs.has(note.id)) return false;
    if (searchFilter === "code" && !note.content.includes("```")) return false;
    if (searchFilter === "source" && !sourceNoteIDs.has(note.id)) return false;
    if (searchFilter === "conflicts" && !isConflict(note)) return false;
    if (!matchesTimeFilter(note.createdAt, timeFilter)) return false;
    const source = sourceByNote.get(note.id);
    const haystack = normalizeForSearch([note.title, note.content, source?.title, source?.url, ...(attachmentNamesByNote.get(note.id) ?? [])].filter(Boolean).join("\n"));
    return terms.every(term => haystack.includes(term));
  }).sort((left, right) => terms.length ? searchScore(right, terms, sourceByNote.get(right.id), attachmentNamesByNote.get(right.id)) - searchScore(left, terms, sourceByNote.get(left.id), attachmentNamesByNote.get(left.id)) || right.updatedAt.localeCompare(left.updatedAt) : right.updatedAt.localeCompare(left.updatedAt));
  const searchHints = new Map(visible.map(note => [note.id, searchMatchHint(terms, sourceByNote.get(note.id), attachmentNamesByNote.get(note.id))]).filter((item): item is [string, string] => Boolean(item[1])));
  const keyboardSelectedID = query.trim() && visible.length ? visible[Math.min(searchSelection, visible.length - 1)]?.id : undefined;
  useEffect(() => {
    if (!keyboardSelectedID) return;
    document.querySelector(`[data-note-id="${CSS.escape(keyboardSelectedID)}"]`)?.scrollIntoView({ block: "nearest" });
  }, [keyboardSelectedID]);

  function navigate(route: Route, replace = false) {
    const path = routePath(route);
    if (replace) history.replaceState(null, "", path); else if (path !== window.location.pathname) history.pushState(null, "", path);
    setView(route.view); setSelectedID(route.noteID); setEditing(false);
  }
  function showView(nextView: View, replace = false) { navigate({ view: nextView }, replace); }
  function showNote(noteID: string) { navigate({ view, noteID }); }

  async function syncNow() {
    if (!authenticated || !navigator.onLine) return;
    try {
      const count = await syncClient.sync();
      const syncedAt = now();
      setLastSyncSuccess(syncedAt);
      void writeLibraryMetadata(lastSyncSuccessKey, syncedAt).catch(() => undefined);
      setSyncStatus(count ? `已同步 ${count} 条本机变更。` : `已同步 · ${new Date(syncedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`);
    }
    catch (error) {
      const message = error instanceof Error ? error.message : "同步失败";
      const failure = { message, at: now() };
      setLastSyncError(failure);
      void writeLibraryMetadata(lastSyncErrorKey, failure).catch(() => undefined);
      setSyncStatus(message);
      if (message === "需要登录") { setAuthenticated(false); localStorage.removeItem("thoughtglean.owner-authenticated"); }
    }
  }
  async function capture() {
    if (!draft.trim() || captureBusy) return;
    setCaptureBusy(true);
    try {
      const note = await createNote(draft, continuedFromID);
      if (captureSource) {
        const source = { noteId: note.id, url: captureSource, title: "", updatedAt: now() };
        await db.sources.put(source);
        await queue({ kind: "source.upsert", noteSyncId: note.syncId, source });
      }
      setDraft(""); setContinuedFromID(undefined); await deleteLibraryMetadata(homeDraftKey);
      setSuggestedSource(undefined); setCaptureSource(undefined); setDraftStatus("saved");
      setToast({ message: "记录已保存", actions: [
        { label: "打开", run: () => showNote(note.id) },
        { label: "撤销", run: () => { void update(note, { deletedAt: now() }); setToast({ message: "已移到回收站" }); } },
      ] });
      requestAnimationFrame(() => captureInput.current?.focus());
      void syncNow();
    } catch (error) {
      setToast({ message: error instanceof Error ? `保存失败：${error.message}` : "保存失败，草稿仍已保留", persistent: true });
    } finally { setCaptureBusy(false); }
  }
  async function update(note: Note, patch: Partial<Note>) {
    const next = { ...note, ...patch, revision: note.revision + 1, updatedAt: now() };
    await saveNote(next); void syncNow();
  }
  async function remove(note: Note) { await update(note, { deletedAt: now() }); showView(view); }
  async function restore(note: Note) { await update(note, { deletedAt: undefined }); }
  async function addImages(files: FileList | File[]) {
    if (!selected) return;
    const { accepted, rejected } = acceptedImages(files);
    try {
      for (const [index, sourceFile] of accepted.entries()) {
        setToast({ message: `正在处理图片 ${index + 1}/${accepted.length}…`, persistent: true });
        const file = await prepareImage(sourceFile);
        const item: Attachment = { id: newID(), syncId: newID(), noteId: selected.id, originalName: file.name || "image", mimeType: file.type, byteSize: file.size, createdAt: now(), blob: file };
        await saveAttachment(selected.syncId, item);
      }
      if (accepted.length) {
        if (navigator.onLine) { setToast({ message: `正在同步 ${accepted.length} 张图片…`, persistent: true }); await syncNow(); }
        setToast({ message: `已添加 ${accepted.length} 张图片${rejected ? `，${rejected} 张格式或大小不支持` : ""}` });
      }
      else if (rejected) setToast({ message: "仅支持 JPEG、PNG、WebP、GIF；手机照片会自动压缩，GIF 需不超过 20 MiB", persistent: true });
    } catch (error) {
      setToast({ message: error instanceof Error ? `图片保存失败：${error.message}` : "图片保存失败", persistent: true });
    }
    if (!accepted.length) void syncNow();
  }
  function pastedImages(event: ClipboardEvent<HTMLTextAreaElement>) {
    const images = [...event.clipboardData.files].filter(file => file.type.startsWith("image/"));
    if (images.length) { event.preventDefault(); void addImages(images); }
  }
  async function capturePastedImages(event: ClipboardEvent<HTMLTextAreaElement>) {
    const pastedText = event.clipboardData.getData("text/plain");
    const pastedURL = pastedText.match(/https?:\/\/[^\s<>"']+/i)?.[0];
    if (pastedURL) setSuggestedSource(pastedURL);
    const pastedFiles = [...event.clipboardData.files].filter(file => file.type.startsWith("image/"));
    const { accepted: images, rejected } = acceptedImages(pastedFiles);
    if (!images.length) {
      if (rejected) { event.preventDefault(); setToast({ message: "无法粘贴图片：仅支持 JPEG、PNG、WebP、GIF；文件可能过大或格式不受支持", persistent: true }); }
      return;
    }
    event.preventDefault();
    try {
      setToast({ message: `正在处理粘贴的 ${images.length} 张图片…`, persistent: true });
      const prepared = [] as File[];
      for (const file of images) prepared.push(await prepareImage(file));
      const note = await createNote(draft || "图片记录", continuedFromID); setDraft(""); setContinuedFromID(undefined); showNote(note.id); setEditing(true);
      for (const file of prepared) {
        const item: Attachment = { id: newID(), syncId: newID(), noteId: note.id, originalName: file.name || "pasted-image.png", mimeType: file.type, byteSize: file.size, createdAt: now(), blob: file };
        await saveAttachment(note.syncId, item);
      }
      setToast({ message: `已粘贴 ${prepared.length} 张图片` }); void syncNow();
    } catch (error) { setToast({ message: error instanceof Error ? `图片粘贴失败：${error.message}` : "图片粘贴失败", persistent: true }); }
  }
  async function importSharedItems(items: SharedItem[]) {
    if (!items.length) return;
    let imageNote: Note | undefined;
    for (const shared of items) {
      const parts = [shared.title.trim(), shared.text.trim(), shared.url.trim()].filter((part, index, values) => part && values.indexOf(part) === index);
      const note = await createNote(parts.join("\n\n") || "图片记录");
      if (shared.images.length) {
        imageNote = note;
        for (const sourceFile of acceptedImages(shared.images).accepted) {
          const file = await prepareImage(sourceFile);
          const item: Attachment = { id: newID(), syncId: newID(), noteId: note.id, originalName: file.name || "shared-image", mimeType: file.type, byteSize: file.size, createdAt: now(), blob: file };
          await saveAttachment(note.syncId, item);
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
    <header className="topbar"><button className="brand" onClick={() => showView("recent")}><span className="brand-mark" /><span className="brand-copy"><strong>拾念</strong></span></button><label className="search-box"><span className="search-icon" aria-hidden="true" /><input ref={searchInput} value={query} onChange={event => setQuery(event.target.value)} onKeyDown={event => {
      if (event.key === "ArrowDown" && visible.length) { event.preventDefault(); setSearchSelection(current => (current + 1) % visible.length); }
      else if (event.key === "ArrowUp" && visible.length) { event.preventDefault(); setSearchSelection(current => (current - 1 + visible.length) % visible.length); }
      else if (event.key === "Enter" && keyboardSelectedID) { event.preventDefault(); showNote(keyboardSelectedID); }
      else if (event.key === "Escape" && query) { event.preventDefault(); setQuery(""); }
    }} type="search" placeholder="搜索标题、正文、来源和图片名" /><kbd>↑↓</kbd><kbd>↵</kbd></label><button className="button button-primary top-new" onClick={() => { showView(view === "trash" ? "recent" : view); requestAnimationFrame(() => captureInput.current?.focus()); }}><span>＋</span><span className="button-label">新记录</span></button></header>
    <div className="workspace"><aside className="sidebar"><nav className="primary-nav">{([ ["recent", "最近"], ["starred", "星标"], ["all", "全部"], ["trash", "回收站"] ] as [View, string][]).map(([key, label]) => <button key={key} className={`nav-item ${view === key ? "active" : ""}`} onClick={() => showView(key)}>{label}</button>)}</nav><button className="nav-item settings-nav" onClick={() => setSettingsOpen(true)}>设置</button><div className="storage-note"><span className="status-dot" /><span>{syncStatus}<small>本机离线副本 · 服务端同步</small></span></div></aside>
      <main className="main-content">{selected ? <Detail note={selected} notes={notes} source={source} attachments={attachments} editing={editing} setEditing={setEditing} onBack={() => showView(view)} onSelect={showNote} onContinue={() => { setContinuedFromID(selected.id); showView(view); }} saveSource={saveSource} update={update} remove={remove} restore={restore} addImages={addImages} pastedImages={pastedImages} /> : selectedID && loadedNotes ? <RouteNotFound onBack={() => showView(view, true)} /> : <Library view={view} notes={visible} query={query} setQuery={setQuery} recentSearches={recentSearches} clearRecentSearches={() => { setRecentSearches([]); void deleteLibraryMetadata(recentSearchesKey); }} searchHints={searchHints} keyboardSelectedID={keyboardSelectedID} searchFilter={searchFilter} setSearchFilter={setSearchFilter} timeFilter={timeFilter} setTimeFilter={setTimeFilter} conflictCount={conflictNotes.length} openFirstConflict={() => conflictNotes[0] && showNote(conflictNotes[0].id)} draft={draft} draftStatus={draftStatus} suggestedSource={suggestedSource} captureSource={captureSource} acceptSource={() => { setCaptureSource(suggestedSource); setSuggestedSource(undefined); }} dismissSource={() => setSuggestedSource(undefined)} removeSource={() => setCaptureSource(undefined)} continuedFromID={continuedFromID} setDraft={setDraft} clearContinuation={() => setContinuedFromID(undefined)} capture={() => void capture()} captureBusy={captureBusy} captureInput={captureInput} paste={event => void capturePastedImages(event)} open={showNote} update={update} restore={restore} />}</main></div>
    {settingsOpen && <div className="modal-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget) setSettingsOpen(false); }}>
      <section className="dialog-card settings-dialog" role="dialog" aria-modal="true" aria-label="设置">
        <header><div><p className="eyebrow">ThoughtGlean</p><h2>设置</h2></div><button className="text-button" onClick={() => setSettingsOpen(false)}>关闭</button></header>
        <div className="settings-section">
          <h3>数据</h3>
          <button className="button button-secondary" onClick={() => void downloadBackup()}>下载完整备份</button>
          <button className="button button-secondary" onClick={() => void downloadMarkdown()}>导出 Markdown</button>
          <button className="button button-secondary" onClick={() => backupInput.current?.click()}>恢复备份</button>
          <input ref={backupInput} hidden type="file" accept="application/json,.json" onChange={event => void importBackup(event)} />
        </div>
        <div className="settings-section">
          <h3>同步与运行状态</h3>
          <p className="muted">{syncStatus}</p>
          <dl className="diagnostic-grid">
            <div><dt>待同步操作</dt><dd>{pendingSyncCount}</dd></div>
            <div><dt>最近同步成功</dt><dd>{formatDiagnosticTime(lastSyncSuccess)}</dd></div>
            <div><dt>本地数据</dt><dd>{notes.filter(note => !note.deletedAt).length} 条记录 · {allAttachments.length} 张图片</dd></div>
            <div><dt>服务器版本</dt><dd>{serverVersion}</dd></div>
          </dl>
          <div className={`diagnostic-error ${lastSyncError ? "has-error" : ""}`}>
            <span>最近同步错误</span>
            <strong>{lastSyncError ? lastSyncError.message : "无"}</strong>
            {lastSyncError && <small>{formatDiagnosticTime(lastSyncError.at)}</small>}
          </div>
          <button className="button button-secondary" onClick={() => void syncNow()}>立即同步</button>
        </div>
        {passkeyEnabled && <div className="settings-section passkey-section">
          <h3>Passkey</h3>
          <p className="muted">{passkeyConfigured ? "使用指纹、面容或设备解锁登录，无需重复输入访问密钥。" : "为这台设备设置快速、安全的登录方式。"}</p>
          {passkeys.map((item, index) => <div className="passkey-row" key={item.id}><span><strong>Passkey {index + 1}</strong><small>添加于 {new Date(item.createdAt).toLocaleDateString()}</small></span>{passkeys.length > 1 && <button className="text-button danger" onClick={() => void removePasskey(item)}>删除</button>}</div>)}
          <button className="button button-secondary" disabled={passkeyBusy} onClick={() => void setupPasskey()}>{passkeyBusy ? "等待设备验证…" : passkeyConfigured ? "添加备用 Passkey" : "设置 Passkey"}</button>
          {passkeyMessage && <p className={passkeyMessage.includes("失败") || passkeyMessage.includes("无法") ? "inline-error" : "inline-success"}>{passkeyMessage}</p>}
        </div>}
        <div className="settings-section settings-danger"><button className="button button-secondary" onClick={() => void signOut()}>退出登录</button></div>
      </section>
    </div>}
    {toast && <aside className="toast" role="status"><span>{toast.message}</span>{toast.actions?.map(action => <button key={action.label} onClick={() => { action.run(); setToast(undefined); }}>{action.label}</button>)}</aside>}
  </div>;
}

const viewTitles: Record<View, string> = { recent: "最近", starred: "星标", all: "全部记录", trash: "回收站" };
function formatDiagnosticTime(value?: string) {
  if (!value) return "尚未同步";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "未知" : date.toLocaleString([], { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}
const isConflict = (note: Note) => note.title.startsWith("同步冲突：") || note.title === "同步冲突记录";
const supportedImageTypes = new Set(["image/jpeg", "image/png", "image/webp", "image/gif"]);
const maxImageBytes = 20 * 1024 * 1024;
const maxOriginalImageBytes = 60 * 1024 * 1024;
function acceptedImages(files: FileList | File[]) {
  const all = Array.from(files);
  const accepted = all.filter(file => supportedImageTypes.has(file.type) && file.size > 0 && file.size <= (file.type === "image/gif" ? maxImageBytes : maxOriginalImageBytes));
  return { accepted, rejected: all.length - accepted.length };
}

async function prepareImage(file: File) {
  if (file.type === "image/gif") return file;
  const orientation = file.type === "image/jpeg" ? await jpegOrientation(file).catch(() => 1) : 1;
  const bitmap = await createImageBitmap(file, { imageOrientation: "from-image" });
  const longest = Math.max(bitmap.width, bitmap.height);
  const mustResize = longest > 2560;
  const mustCompress = file.size > 4 * 1024 * 1024;
  if (!mustResize && !mustCompress && orientation === 1) { bitmap.close(); return file; }
  const scale = mustResize ? 2560 / longest : 1;
  const canvas = document.createElement("canvas");
  canvas.width = Math.max(1, Math.round(bitmap.width * scale)); canvas.height = Math.max(1, Math.round(bitmap.height * scale));
  const context = canvas.getContext("2d");
  if (!context) { bitmap.close(); throw new Error("当前浏览器无法处理这张图片"); }
  context.drawImage(bitmap, 0, 0, canvas.width, canvas.height); bitmap.close();
  const outputType = file.type === "image/png" && file.size <= 10 * 1024 * 1024 ? "image/png" : file.type === "image/png" ? "image/webp" : file.type;
  const blob = await new Promise<Blob>((resolve, reject) => canvas.toBlob(value => value ? resolve(value) : reject(new Error("图片压缩失败")), outputType, .86));
  if (blob.size > maxImageBytes) throw new Error("压缩后仍超过 20 MiB，请先缩小图片");
  const extension = outputType === "image/webp" ? ".webp" : outputType === "image/jpeg" ? ".jpg" : ".png";
  const name = outputType === file.type ? file.name : file.name.replace(/\.[^.]+$/, "") + extension;
  return new File([blob], name, { type: outputType, lastModified: file.lastModified });
}

async function jpegOrientation(file: File) {
  const view = new DataView(await file.slice(0, 128 * 1024).arrayBuffer());
  if (view.byteLength < 4 || view.getUint16(0) !== 0xffd8) return 1;
  let offset = 2;
  while (offset + 4 < view.byteLength) {
    const marker = view.getUint16(offset); offset += 2;
    const length = view.getUint16(offset); offset += 2;
    if (marker === 0xffe1 && offset + length - 2 <= view.byteLength && view.getUint32(offset) === 0x45786966) {
      const tiff = offset + 6; const little = view.getUint16(tiff) === 0x4949;
      const firstIFD = tiff + view.getUint32(tiff + 4, little);
      const entries = view.getUint16(firstIFD, little);
      for (let index = 0; index < entries; index += 1) {
        const entry = firstIFD + 2 + index * 12;
        if (entry + 12 <= view.byteLength && view.getUint16(entry, little) === 0x0112) return view.getUint16(entry + 8, little);
      }
      return 1;
    }
    if (length < 2) break;
    offset += length - 2;
  }
  return 1;
}
const normalizeForSearch = (value: string) => value.normalize("NFKC").toLocaleLowerCase();
const searchTerms = (value: string) => normalizeForSearch(value).split(/[\s\p{P}\p{S}]+/u).filter(Boolean);

function matchesTimeFilter(value: string, filter: TimeFilter) {
  if (filter === "any") return true;
  const timestamp = new Date(value).getTime();
  const nowValue = new Date();
  if (filter === "today") {
    const start = new Date(nowValue.getFullYear(), nowValue.getMonth(), nowValue.getDate()).getTime();
    return timestamp >= start;
  }
  const days = filter === "week" ? 7 : 30;
  return timestamp >= nowValue.getTime() - days * 24 * 60 * 60 * 1000;
}

function searchScore(note: Note, terms: string[], source?: { title: string; url: string }, attachmentNames: string[] = []) {
  const title = normalizeForSearch(note.title);
  const content = normalizeForSearch(note.content);
  const related = normalizeForSearch([source?.title, source?.url, ...attachmentNames].filter(Boolean).join("\n"));
  return terms.reduce((score, term) => score + (title === term ? 100 : title.startsWith(term) ? 50 : title.includes(term) ? 30 : 0) + (content.includes(term) ? 10 : 0) + (related.includes(term) ? 5 : 0), 0);
}

function searchMatchHint(terms: string[], source?: { title: string; url: string }, attachmentNames: string[] = []) {
  if (!terms.length) return "";
  const sourceLabel = source?.title || source?.url || "";
  if (sourceLabel && terms.some(term => normalizeForSearch(`${source?.title}\n${source?.url}`).includes(term))) return `来源：${sourceLabel}`;
  const image = attachmentNames.find(name => terms.some(term => normalizeForSearch(name).includes(term)));
  return image ? `图片：${image}` : "";
}

function excerptForSearch(content: string, query: string) {
  const terms = searchTerms(query);
  if (!terms.length) return content;
  const normalized = normalizeForSearch(content);
  const positions = terms.map(term => normalized.indexOf(term)).filter(position => position >= 0);
  if (!positions.length) return content;
  const position = Math.min(...positions);
  const start = Math.max(0, position - 46);
  const end = Math.min(content.length, position + 150);
  return `${start ? "…" : ""}${content.slice(start, end)}${end < content.length ? "…" : ""}`;
}

function HighlightedText({ text, query }: { text: string; query: string }) {
  const terms = searchTerms(query);
  if (!terms.length) return text;
  const escaped = terms.map(term => term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const matcher = new RegExp(`(${escaped.join("|")})`, "gi");
  return <>{text.split(matcher).map((part, index) => terms.some(term => part.toLocaleLowerCase() === term.toLocaleLowerCase()) ? <mark key={index}>{part}</mark> : part)}</>;
}

function RouteNotFound({ onBack }: { onBack: () => void }) {
  return <section className="route-not-found"><p className="eyebrow">404</p><h1>这条记录不存在</h1><p className="muted">它可能已被其他设备永久移除，或者链接不完整。</p><button className="button button-primary" onClick={onBack}>返回记录列表</button></section>;
}

function Library({ view, notes, query, setQuery, recentSearches, clearRecentSearches, searchHints, keyboardSelectedID, searchFilter, setSearchFilter, timeFilter, setTimeFilter, conflictCount, openFirstConflict, draft, draftStatus, suggestedSource, captureSource, acceptSource, dismissSource, removeSource, continuedFromID, setDraft, clearContinuation, capture, captureBusy, captureInput, paste, open, update, restore }: { view: View; notes: Note[]; query: string; setQuery: (value: string) => void; recentSearches: string[]; clearRecentSearches: () => void; searchHints: Map<string, string>; keyboardSelectedID?: string; searchFilter: SearchFilter; setSearchFilter: (value: SearchFilter) => void; timeFilter: TimeFilter; setTimeFilter: (value: TimeFilter) => void; conflictCount: number; openFirstConflict: () => void; draft: string; draftStatus: "idle" | "saving" | "saved"; suggestedSource?: string; captureSource?: string; acceptSource: () => void; dismissSource: () => void; removeSource: () => void; continuedFromID?: string; setDraft: (value: string) => void; clearContinuation: () => void; capture: () => void; captureBusy: boolean; captureInput: RefObject<HTMLTextAreaElement | null>; paste: (event: ClipboardEvent<HTMLTextAreaElement>) => void; open: (noteID: string) => void; update: (note: Note, patch: Partial<Note>) => Promise<void>; restore: (note: Note) => Promise<void> }) {
  useLayoutEffect(() => { fitCaptureTextarea(captureInput.current); }, [captureInput, draft]);
  useEffect(() => {
    const resize = () => fitCaptureTextarea(captureInput.current);
    window.addEventListener("resize", resize);
    return () => window.removeEventListener("resize", resize);
  }, [captureInput]);

  const groups = new Map<string, Note[]>();
  for (const note of notes) {
    const key = localDateKey(note.createdAt);
    groups.set(key, [...(groups.get(key) ?? []), note]);
  }
  return <section className="library-view">
    <header className="view-heading"><h1>{viewTitles[view]}</h1><p className="muted">{notes.length ? `${notes.length} 条记录` : ""}</p></header>
    <div className="search-filter-rows"><div className="search-filters" aria-label="内容筛选">{([['all', '全部'], ['images', '图片'], ['code', '代码'], ['source', '来源'], ['conflicts', '冲突']] as [SearchFilter, string][]).map(([key, label]) => <button key={key} aria-pressed={searchFilter === key} onClick={() => setSearchFilter(key)}>{label}</button>)}</div><div className="search-filters time-filters" aria-label="时间筛选">{([['any', '不限时间'], ['today', '今天'], ['week', '最近一周'], ['month', '最近一月']] as [TimeFilter, string][]).map(([key, label]) => <button key={key} aria-pressed={timeFilter === key} onClick={() => setTimeFilter(key)}>{label}</button>)}</div></div>
    {!query && recentSearches.length > 0 && <aside className="recent-searches"><span>最近搜索</span>{recentSearches.map(item => <button key={item} onClick={() => setQuery(item)}>{item}</button>)}<button className="clear-searches" onClick={clearRecentSearches}>清除</button></aside>}
    {conflictCount > 0 && searchFilter !== "conflicts" && view !== "trash" && <aside className="conflict-notice"><span><strong>{conflictCount} 条同步冲突待处理</strong><small>原版本和另一设备版本均已保留。</small></span><button className="button button-secondary" onClick={openFirstConflict}>查看</button></aside>}
    {view !== "trash" && <form className={`capture-card ${/^\s*#\s+\S/.test(draft) ? "has-title-line" : ""}`} onSubmit={event => { event.preventDefault(); capture(); }}>
      {continuedFromID && <div className="continuation-chip"><span>续自记录</span><button type="button" aria-label="取消续写" onClick={clearContinuation}>×</button></div>}
      {captureSource && <div className="source-suggestion accepted"><span>来源：{captureSource}</span><button type="button" onClick={removeSource}>移除</button></div>}
      <textarea ref={captureInput} id="captureInput" value={draft} onChange={event => setDraft(event.target.value)} onPaste={paste} onKeyDown={event => { if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); capture(); } }} placeholder={continuedFromID ? "继续写…" : "写下想法…"} />
      {suggestedSource && !captureSource && <aside className="source-suggestion"><span>识别到链接，设为这条记录的来源？<small>{suggestedSource}</small></span><div><button type="button" onClick={dismissSource}>忽略</button><button type="button" onClick={acceptSource}>设为来源</button></div></aside>}
      <footer className="capture-footer"><span className="draft-state"><i className={draftStatus} />{draftStatus === "saving" ? "正在保存草稿…" : draftStatus === "saved" ? "草稿已保存" : "草稿自动保存"} · 首行 <code># 标题</code> 可设置标题 · 按 <kbd>N</kbd> 新记录</span><div className="capture-actions"><span className="shortcut-hint"><kbd>⌘</kbd><kbd>↵</kbd> 保存</span><button className="button button-primary" disabled={captureBusy || !draft.trim()}>{captureBusy ? "保存中…" : "保存"}</button></div></footer>
    </form>}
    {notes.length ? <div className="timeline">{[...groups].map(([key, group]) => <DateGroup key={key} dateKey={key} notes={group} query={query} searchHints={searchHints} keyboardSelectedID={keyboardSelectedID} view={view} open={open} update={update} restore={restore} />)}</div> : <section className="empty-state"><h2>{view === "trash" ? "回收站是空的" : view === "starred" ? "还没有星标" : query || searchFilter !== "all" || timeFilter !== "any" ? "没有匹配的记录" : "还没有记录"}</h2><p>{view === "trash" ? "删除的记录会保留在这里，可随时恢复。" : query || searchFilter !== "all" || timeFilter !== "any" ? "换个关键词或筛选条件试试。" : "写下第一条内容。"}</p></section>}
  </section>;
}

function fitCaptureTextarea(textarea: HTMLTextAreaElement | null) {
  if (!textarea) return;
  textarea.style.height = "auto";
  textarea.style.height = `${Math.min(textarea.scrollHeight, captureMaxHeight)}px`;
  textarea.style.overflowY = textarea.scrollHeight > captureMaxHeight ? "auto" : "hidden";
}

function DateGroup({ dateKey, notes, query, searchHints, keyboardSelectedID, view, open, update, restore }: { dateKey: string; notes: Note[]; query: string; searchHints: Map<string, string>; keyboardSelectedID?: string; view: View; open: (noteID: string) => void; update: (note: Note, patch: Partial<Note>) => Promise<void>; restore: (note: Note) => Promise<void> }) {
  const heading = dateHeading(dateKey);
  return <section className="date-group"><h2 className="date-heading">{heading.label}<small>{heading.detail}</small></h2><div className="date-notes">{notes.map(note => <article className={`note-row ${keyboardSelectedID === note.id ? "keyboard-selected" : ""}`} data-note-id={note.id} key={note.id}>
    <button className="note-row-open" onClick={() => open(note.id)}><time className="note-time">{new Date(note.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</time><span className="note-row-main"><strong className="note-row-title"><HighlightedText text={note.title} query={query} /></strong><span className="note-excerpt"><HighlightedText text={excerptForSearch(note.content, query)} query={query} /></span>{(note.continuedFromId || searchHints.has(note.id)) && <span className="note-meta">{note.continuedFromId && <span className="relation-label">{isConflict(note) ? "同步冲突" : "续写记录"}</span>}{searchHints.get(note.id) && <span><HighlightedText text={searchHints.get(note.id)!} query={query} /></span>}</span>}</span></button>
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
  const [mergingConflict, setMergingConflict] = useState(false);
  const titleEditor = useRef<HTMLInputElement>(null);
  const editor = useRef<HTMLTextAreaElement>(null);
  const editFocus = useRef<"title" | "body">("body");
  const editDraftKey = `draft.note.${note.syncId}`;
  const conflictParent = isConflict(note) && note.continuedFromId ? notes.find(item => item.id === note.continuedFromId) : undefined;
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
  const save = async () => {
    if (mergingConflict && conflictParent) {
      await update(conflictParent, { title: title.trim() || "未命名记录", content });
      await update(note, { deletedAt: now() });
      onSelect(conflictParent.id);
    } else {
      await update(note, { title: title.trim() || "未命名记录", content });
    }
    await deleteLibraryMetadata(editDraftKey); setHasEditDraft(false); setMergingConflict(false); setEditing(false);
  };
  const cancel = () => { setTitle(note.title); setContent(note.content); setHasEditDraft(false); setMergingConflict(false); void deleteLibraryMetadata(editDraftKey); setEditing(false); };
  const editorKeyDown = (event: KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    if (event.key === "Escape") { event.preventDefault(); cancel(); }
    else if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); void save(); }
  };
  const beginEditing = (focus: "title" | "body" = "body") => { if (!note.deletedAt) { editFocus.current = focus; setEditing(true); } };
  const beginConflictMerge = () => {
    if (!conflictParent) return;
    setMergingConflict(true); setTitle(conflictParent.title);
    setContent(conflictParent.content === note.content ? conflictParent.content : `${conflictParent.content}\n\n--- 另一设备版本 ---\n\n${note.content}`);
    editFocus.current = "body"; setEditing(true);
  };
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
  const resolveConflict = async (replaceOriginal: boolean) => {
    if (!conflictParent) return;
    if (replaceOriginal) {
      const cleanTitle = note.title.replace(/^同步冲突：/, "");
      await update(conflictParent, { title: cleanTitle || "未命名记录", content: note.content, starred: note.starred });
    }
    await update(note, { deletedAt: now() });
    onSelect(conflictParent.id);
  };
  const currentIndex = activeNotes.findIndex(item => item.id === note.id);
  const context = activeNotes.slice(Math.max(0, currentIndex - 2), currentIndex + 3);
  return <section className="detail-view"><header className="note-toolbar"><button className="text-button" onClick={onBack}>← 返回</button><div className="note-toolbar-actions"><button className="text-button" onClick={() => void update(note, { starred: !note.starred })}>{note.starred ? "★ 已星标" : "☆ 星标"}</button>{note.deletedAt ? <button className="text-button" onClick={() => void restore(note)}>恢复</button> : <button className="text-button danger" onClick={() => void remove(note)}>移到回收站</button>}</div></header>
    <div className="note-detail-layout"><article className="note-paper">{isConflict(note) && <aside className="conflict-detail"><span><strong>{mergingConflict ? "合并两个版本" : "检测到另一设备的并发修改"}</strong><small>{mergingConflict ? "编辑合并后的正文并保存；完成后冲突记录会自动移入回收站。" : conflictParent ? `原版本“${conflictParent.title}”仍然保留，请明确选择处理方式。` : "原版本仍然保留，可编辑本记录完成手动合并。"}</small></span>{!editing && !note.deletedAt && <div className="conflict-actions">{conflictParent && <><button className="button button-secondary" onClick={() => void resolveConflict(false)}>保留当前</button><button className="button button-secondary" onClick={() => void resolveConflict(true)}>使用另一版本</button><button className="button button-primary" onClick={beginConflictMerge}>合并后保存</button></>}</div>}</aside>}<div className="note-origin"><time>{new Date(note.createdAt).toLocaleString()}</time><span>{editing ? mergingConflict ? "正在合并" : "编辑中" : "已保存在本机"}</span></div>
      {editing ? <><input ref={titleEditor} className="note-title-input" value={title} onChange={event => setTitle(event.target.value)} onKeyDown={editorKeyDown} placeholder="标题（可选）" /><textarea ref={editor} className="note-editor" value={content} onChange={event => setContent(event.target.value)} onPaste={pastedImages} onKeyDown={editorKeyDown} /><div className="editor-tools"><button className="button button-secondary" onClick={insertCode}>插入代码片段</button><label className="button button-secondary">添加图片<input hidden type="file" accept="image/*" multiple onChange={event => event.target.files && void addImages(event.target.files)} /></label><button className="button button-secondary" onClick={() => void editSource()}>关联来源</button></div><div className="edit-actions"><span className="shortcut-hint"><kbd>Esc</kbd> 取消 · <kbd>⌘</kbd><kbd>↵</kbd> 保存</span><button className="button button-ghost" onClick={cancel}>取消</button><button className="button button-primary" onClick={() => void save()}>{mergingConflict ? "保存并解决冲突" : "保存修改"}</button></div></> : conflictParent ? <section className="conflict-compare"><article><header><strong>当前版本</strong><time>{new Date(conflictParent.updatedAt).toLocaleString()}</time></header><h2>{conflictParent.title}</h2><div className="note-rendered"><LimitedMarkdown content={conflictParent.content} /></div></article><article><header><strong>另一设备版本</strong><time>{new Date(note.updatedAt).toLocaleString()}</time></header><h2>{note.title.replace(/^同步冲突：/, "")}</h2><div className="note-rendered"><LimitedMarkdown content={note.content} /></div></article></section> : <><div className="note-edit-surface note-title-surface" role="button" tabIndex={note.deletedAt ? -1 : 0} aria-label="编辑标题和正文" onClick={() => beginEditing("title")} onKeyDown={event => { if (event.key === "Enter") beginEditing("title"); }}><h1 className="note-title">{note.title}</h1></div><div className="note-edit-surface note-rendered" role="button" tabIndex={note.deletedAt ? -1 : 0} aria-label="编辑正文" onClick={activateBodyEditor} onKeyDown={event => { if (event.key === "Enter") beginEditing(); }}><LimitedMarkdown content={note.content} /></div></>}
      {source && <p className="note-source">来源：<a href={source.url} target="_blank" rel="noreferrer">{source.title || source.url}</a></p>}
      <section className="attachments">{attachments.map(item => <AttachmentPreview key={item.id} item={item} onOpen={() => setLightbox(item.id)} onEdit={editing ? async () => { const value = prompt("图片说明或替代文本（可选）", item.altText ?? ""); if (value !== null) await updateAttachmentAlt(item, value.trim().slice(0, 500)); } : undefined} onDelete={editing ? async () => { await deleteAttachment(item); } : undefined} />)}</section>
    </article><aside className="context-rail"><h2>相关记录</h2><p className="context-copy">这条记录前后的内容。</p><div className="context-timeline">{context.map(item => <div className={`context-item ${item.id === note.id ? "current" : ""}`} key={item.id}><button onClick={() => item.id !== note.id && onSelect(item.id)}><time>{new Date(item.createdAt).toLocaleString()}</time><span>{item.title}</span></button></div>)}</div>{!note.deletedAt && <button className="button button-secondary button-wide" onClick={onContinue}>继续写</button>}</aside></div>
    {lightbox && <ImageLightbox items={attachments} selectedID={lightbox} onSelect={setLightbox} onClose={() => setLightbox(undefined)} />}
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

function AttachmentPreview({ item, onOpen, onEdit, onDelete }: { item: Attachment; onOpen: () => void; onEdit?: () => Promise<void>; onDelete?: () => Promise<void> }) {
  const [url, setURL] = useState("");
  useEffect(() => { const value = URL.createObjectURL(item.blob); setURL(value); return () => URL.revokeObjectURL(value); }, [item.blob]);
  return <span className="attachment-thumb"><button onClick={onOpen}><img src={url} alt={item.altText || item.originalName} loading="lazy" /></button><small>{item.altText || item.originalName}</small>{onEdit && <button className="attachment-caption" aria-label={`编辑 ${item.originalName} 的图片说明`} onClick={() => void onEdit()}>说明</button>}{onDelete && <button className="attachment-delete" aria-label={`移除 ${item.originalName}`} onClick={() => void onDelete()}>×</button>}</span>;
}

function ImageLightbox({ items, selectedID, onSelect, onClose }: { items: Attachment[]; selectedID: string; onSelect: (id: string) => void; onClose: () => void }) {
  const index = Math.max(0, items.findIndex(item => item.id === selectedID));
  const item = items[index];
  const [url, setURL] = useState("");
  const [copyState, setCopyState] = useState("复制");
  const selectOffset = (offset: number) => {
    if (items.length < 2) return;
    onSelect(items[(index + offset + items.length) % items.length].id);
  };
  useEffect(() => {
    if (!item) return;
    const value = URL.createObjectURL(item.blob);
    setURL(value);
    return () => URL.revokeObjectURL(value);
  }, [item]);
  useEffect(() => {
    const keydown = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") onClose();
      else if (event.key === "ArrowLeft") selectOffset(-1);
      else if (event.key === "ArrowRight") selectOffset(1);
    };
    addEventListener("keydown", keydown);
    return () => removeEventListener("keydown", keydown);
  }, [index, items]);
  if (!item) return null;
  const downloadImage = () => {
    const link = document.createElement("a");
    link.href = url; link.download = item.originalName || "thoughtglean-image"; link.click();
  };
  const copyImage = async () => {
    try {
      if (!navigator.clipboard?.write || typeof ClipboardItem === "undefined") throw new Error("当前浏览器不支持复制图片");
      let blob = item.blob;
      if (blob.type !== "image/png") {
        const bitmap = await createImageBitmap(blob); const canvas = document.createElement("canvas");
        canvas.width = bitmap.width; canvas.height = bitmap.height; canvas.getContext("2d")?.drawImage(bitmap, 0, 0); bitmap.close();
        blob = await new Promise<Blob>((resolve, reject) => canvas.toBlob(value => value ? resolve(value) : reject(new Error("无法转换图片")), "image/png"));
      }
      await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]); setCopyState("已复制");
      window.setTimeout(() => setCopyState("复制"), 1800);
    } catch (error) { setCopyState(error instanceof Error ? error.message : "复制失败"); }
  };
  return <div className="image-lightbox" role="dialog" aria-modal="true" aria-label={`查看图片 ${item.originalName}`} onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}>
    <header><span>{index + 1} / {items.length} · {item.originalName} · {formatBytes(item.byteSize)}</span><div><button onClick={() => void copyImage()}>{copyState}</button><button onClick={downloadImage}>下载原图</button><button autoFocus onClick={onClose}>关闭</button></div></header>
    {items.length > 1 && <button className="lightbox-previous" aria-label="上一张图片" onClick={() => selectOffset(-1)}>‹</button>}
    <img src={url} alt={item.originalName} />
    {items.length > 1 && <button className="lightbox-next" aria-label="下一张图片" onClick={() => selectOffset(1)}>›</button>}
  </div>;
}

function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
  return `${(value / 1024 / 1024).toFixed(1)} MiB`;
}
