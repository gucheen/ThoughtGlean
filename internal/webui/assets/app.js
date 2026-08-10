const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => Array.from(document.querySelectorAll(selector));

const LEGACY_DRAFT_KEY = "thoughtglean.capture-draft.v1";
const DRAFT_PREFIX = "thoughtglean.capture-draft.v2.";
const ACTIVE_DRAFT_KEY = "thoughtglean.capture-active.v2";
const state = {
  view: "recent",
  query: "",
  notes: [],
  currentNote: null,
  noteContext: null,
  continuedFromId: null,
  requestId: newRequestId(),
  protectedDraft: null,
  saving: false,
  searchSequence: 0,
  editing: false,
  toastTimer: null,
};

function newRequestId() {
  if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
  return `capture-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function draftStorageKey(requestId) {
  return `${DRAFT_PREFIX}${requestId}`;
}

function currentDraft() {
  return {
    content: $("#captureInput").value,
    continuedFromId: state.continuedFromId,
    requestId: state.requestId,
  };
}

function setActiveDraft(requestId) {
  try {
    sessionStorage.setItem(ACTIVE_DRAFT_KEY, requestId);
  } catch {
    // The durable localStorage copy remains the recovery source.
  }
}

function activeDraftRequestId() {
  try {
    return sessionStorage.getItem(ACTIVE_DRAFT_KEY);
  } catch {
    return null;
  }
}

function readStoredDraft(requestId) {
  if (!requestId) return null;
  const draft = JSON.parse(localStorage.getItem(draftStorageKey(requestId)) || "null");
  return draft && typeof draft.content === "string" ? draft : null;
}

function newestStoredDraft() {
  let newest = null;
  for (let index = 0; index < localStorage.length; index += 1) {
    const key = localStorage.key(index);
    if (!key?.startsWith(DRAFT_PREFIX)) continue;
    try {
      const draft = JSON.parse(localStorage.getItem(key) || "null");
      if (draft && typeof draft.content === "string" && (!newest || (draft.updatedAt || 0) > (newest.updatedAt || 0))) {
        newest = draft;
      }
    } catch {
      // Ignore a damaged slot without risking other drafts.
    }
  }
  return newest;
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(data.error || `请求失败（${response.status}）`);
    error.status = response.status;
    error.data = data;
    throw error;
  }
  return data;
}

function restoreDraft() {
  try {
    const activeRequestId = activeDraftRequestId();
    let draft = readStoredDraft(activeRequestId);
    if (!draft) {
      const legacy = JSON.parse(localStorage.getItem(LEGACY_DRAFT_KEY) || "null");
      if (legacy && typeof legacy.content === "string") {
        draft = { ...legacy, requestId: legacy.requestId || newRequestId(), updatedAt: Date.now() };
        localStorage.setItem(draftStorageKey(draft.requestId), JSON.stringify(draft));
        localStorage.removeItem(LEGACY_DRAFT_KEY);
      }
    }
    if (!draft) draft = newestStoredDraft();
    if (!draft || typeof draft.content !== "string") return;
    $("#captureInput").value = draft.content;
    state.requestId = draft.requestId || newRequestId();
    state.continuedFromId = Number.isInteger(draft.continuedFromId) ? draft.continuedFromId : null;
    state.protectedDraft = currentDraft();
    setActiveDraft(state.requestId);
    updateContinuationChip();
    resizeCapture();
    if (draft.content) $("#draftState").textContent = "草稿已保存在本机";
  } catch {
    $("#draftState").textContent = "无法读取本地草稿；请勿关闭页面";
  }
}

let draftTimer;
function storeDraft(draft) {
  try {
    if (!draft.content && !draft.continuedFromId) {
      localStorage.removeItem(draftStorageKey(draft.requestId));
      $("#draftState").textContent = "草稿会自动保存在本机";
      return true;
    }
    localStorage.setItem(draftStorageKey(draft.requestId), JSON.stringify({ ...draft, updatedAt: Date.now() }));
    setActiveDraft(draft.requestId);
    $("#draftState").textContent = "草稿已保存在本机";
    return true;
  } catch {
    $("#draftState").textContent = "本地草稿保存失败；请勿关闭页面";
    return false;
  }
}

function removeStoredDraft(requestId) {
  try {
    localStorage.removeItem(draftStorageKey(requestId));
    return true;
  } catch {
    return false;
  }
}

function persistDraft(immediate = false, draft = null) {
  clearTimeout(draftTimer);
  if (immediate) return storeDraft(draft || currentDraft());
  draftTimer = setTimeout(() => {
    storeDraft(currentDraft());
  }, 250);
  return true;
}

function ensureFreshRequestForChangedDraft() {
  const protectedDraft = state.protectedDraft;
  if (!protectedDraft) return;
  const changed = $("#captureInput").value !== protectedDraft.content ||
    state.continuedFromId !== protectedDraft.continuedFromId;
  if (!changed) return;
  state.requestId = newRequestId();
  state.protectedDraft = null;
  setActiveDraft(state.requestId);
}

function resizeCapture() {
  const input = $("#captureInput");
  input.style.height = "auto";
  input.style.height = `${Math.min(input.scrollHeight, 260)}px`;
}

async function submitCapture(event) {
  event?.preventDefault();
  if (state.saving) return;
  const input = $("#captureInput");
  const content = input.value;
  if (!content.trim()) {
    $("#captureError").textContent = "请输入内容。";
    input.focus();
    return;
  }
  const snapshot = { content, continuedFromId: state.continuedFromId, requestId: state.requestId };
  const button = $("#captureSubmit");
  persistDraft(true, snapshot);
  state.saving = true;
  input.readOnly = true;
  button.disabled = true;
  button.textContent = "保存中…";
  $("#captureError").textContent = "";
  $("#draftState").textContent = "正在保存";
  try {
    const result = await api("/api/notes", {
      method: "POST",
      body: JSON.stringify(snapshot),
    });
    const unchanged = input.value === snapshot.content && state.continuedFromId === snapshot.continuedFromId;
    if (unchanged) {
      input.value = "";
      state.continuedFromId = null;
      removeStoredDraft(snapshot.requestId);
      $("#draftState").textContent = "草稿会自动保存在本机";
      updateContinuationChip();
      resizeCapture();
    }
    state.requestId = newRequestId();
    state.protectedDraft = null;
    setActiveDraft(state.requestId);
    if (!unchanged) persistDraft(true);
    showToast(result.duplicate ? "这条记录已保存。" : "已保存。", result.note);
    await loadNotes();
  } catch (error) {
    if (error.status === 409 && error.data?.code === "idempotency_conflict") {
      removeStoredDraft(snapshot.requestId);
      state.requestId = newRequestId();
      state.protectedDraft = null;
      setActiveDraft(state.requestId);
      persistDraft(true);
      $("#captureError").textContent = "保存标识发生冲突，已安全换新；请重试。";
    } else {
      state.protectedDraft = snapshot;
      const locallyStored = persistDraft(true, snapshot);
      $("#captureError").textContent = locallyStored
        ? "尚未保存；草稿仍在本机，请重试。"
        : "尚未保存，本地草稿也不可用；请勿关闭页面并重试。";
      $("#draftState").textContent = locallyStored ? "保存失败，草稿仍在本机" : "本地草稿保存失败；请勿关闭页面";
    }
  } finally {
    state.saving = false;
    input.readOnly = false;
    button.disabled = false;
    button.textContent = "保存";
  }
}

function showToast(message) {
  const toast = $("#toast");
  clearTimeout(state.toastTimer);
  toast.textContent = message;
  toast.hidden = false;
  state.toastTimer = setTimeout(() => { toast.hidden = true; }, 2600);
}

async function loadNotes() {
  const sequence = ++state.searchSequence;
  $("#loadingState").hidden = false;
  $("#timeline").hidden = true;
  $("#emptyState").hidden = true;
  const params = new URLSearchParams({ view: state.view, limit: "100" });
  if (state.query.trim()) params.set("q", state.query.trim());
  try {
    const result = await api(`/api/notes?${params}`);
    if (sequence !== state.searchSequence) return;
    state.notes = Array.isArray(result) ? result : (result.notes || []);
    renderLibrary();
  } catch (error) {
    if (sequence !== state.searchSequence) return;
    $("#timeline").replaceChildren();
    $("#emptyState").hidden = false;
    $("#emptyTitle").textContent = "无法加载记录";
    $("#emptyDescription").textContent = error.message;
  } finally {
    if (sequence === state.searchSequence) $("#loadingState").hidden = true;
  }
}

function renderLibrary() {
  const titleByView = { recent: "最近", all: "全部记录", starred: "星标", trash: "回收站" };
  $("#viewTitle").textContent = state.query ? `搜索：${state.query.trim()}` : titleByView[state.view];
  $("#viewSummary").textContent = state.query ? `${state.notes.length} 条结果` : (state.notes.length ? `${state.notes.length} 条记录` : "");
  $("#captureForm").hidden = state.view === "trash";
  const timeline = $("#timeline");
  timeline.replaceChildren();
  timeline.hidden = state.notes.length === 0;
  if (!state.notes.length) {
    $("#emptyState").hidden = false;
    if (state.query) {
      $("#emptyTitle").textContent = "没有结果";
      $("#emptyDescription").textContent = "换个关键词试试。";
    } else if (state.view === "trash") {
      $("#emptyTitle").textContent = "回收站是空的";
      $("#emptyDescription").textContent = "删除的记录会保留在这里，可随时恢复。";
    } else if (state.view === "starred") {
      $("#emptyTitle").textContent = "还没有星标";
      $("#emptyDescription").textContent = "给重要记录添加星标。";
    } else {
      $("#emptyTitle").textContent = "还没有记录";
      $("#emptyDescription").textContent = "写下第一条内容。";
    }
    return;
  }
  $("#emptyState").hidden = true;
  const groups = new Map();
  for (const note of state.notes) {
    const key = localDateKey(note.createdAt);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(note);
  }
  for (const [key, notes] of groups) timeline.append(renderDateGroup(key, notes));
}

function localDateKey(value) {
  const date = new Date(value);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function dateHeading(key) {
  const date = new Date(`${key}T00:00:00`);
  const today = new Date();
  const todayKey = localDateKey(today.toISOString());
  const yesterday = new Date(today);
  yesterday.setDate(today.getDate() - 1);
  const label = key === todayKey ? "今天" : key === localDateKey(yesterday.toISOString()) ? "昨天" : `${date.getMonth() + 1} 月 ${date.getDate()} 日`;
  const detail = `${date.getFullYear()} 年 · ${["周日", "周一", "周二", "周三", "周四", "周五", "周六"][date.getDay()]}`;
  return { label, detail };
}

function renderDateGroup(key, notes) {
  const section = document.createElement("section");
  section.className = "date-group";
  const heading = document.createElement("h2");
  heading.className = "date-heading";
  const labels = dateHeading(key);
  heading.append(labels.label);
  const detail = document.createElement("small");
  detail.textContent = labels.detail;
  heading.append(detail);
  const list = document.createElement("div");
  list.className = "date-notes";
  notes.forEach((note) => list.append(renderNoteRow(note)));
  section.append(heading, list);
  return section;
}

function renderNoteRow(note) {
  const row = document.createElement("div");
  row.className = "note-row";
  const openButton = document.createElement("button");
  openButton.type = "button";
  openButton.className = "note-row-open";
  openButton.addEventListener("click", () => openNote(note.id));
  const time = document.createElement("time");
  time.className = "note-time";
  time.dateTime = note.createdAt;
  time.textContent = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false }).format(new Date(note.createdAt));
  const main = document.createElement("span");
  main.className = "note-row-main";
  if (note.title) {
    const title = document.createElement("span");
    title.className = "note-row-title";
    title.textContent = note.title;
    main.append(title);
  }
  const excerpt = document.createElement("span");
  excerpt.className = "note-excerpt";
  excerpt.textContent = note.content;
  main.append(excerpt);
  if (note.continuedFromId) {
    const meta = document.createElement("span");
    meta.className = "note-meta";
    const relation = document.createElement("span");
    relation.className = "relation-label";
    relation.textContent = "续自记录";
    meta.append(relation);
    main.append(meta);
  }
  const actions = document.createElement("span");
  actions.className = "row-actions";
  if (state.view === "trash") {
    const restore = document.createElement("button");
    restore.type = "button";
    restore.className = "icon-action restore-action";
    restore.textContent = "恢复";
    restore.addEventListener("click", async (event) => {
      event.stopPropagation();
      await restoreNote(note.id);
    });
    actions.append(restore);
  } else {
    const star = document.createElement("button");
    star.type = "button";
    star.className = `icon-action${note.starred ? " active" : ""}`;
    star.title = note.starred ? "取消星标" : "添加星标";
    star.setAttribute("aria-label", star.title);
    star.textContent = note.starred ? "★" : "☆";
    star.addEventListener("click", async (event) => {
      event.stopPropagation();
      await toggleStar(note);
    });
    actions.append(star);
  }
  openButton.append(time, main);
  row.append(openButton, actions);
  return row;
}

async function toggleStar(note) {
  try {
    const result = await api(`/api/notes/${note.id}`, {
      method: "PATCH",
      body: JSON.stringify({ starred: !note.starred, expectedRevision: note.revision }),
    });
    if (state.currentNote?.id === note.id) state.currentNote = result.note;
    showToast(result.note.starred ? "已添加星标。" : "已取消星标。 ");
    await loadNotes();
    if (state.currentNote?.id === note.id) renderNoteDetail();
  } catch (error) {
    showToast(error.status === 409 ? "这条记录已在别处改变，请重新打开。" : "星标没有保存，请重试。");
  }
}

async function openNote(id, pushHistory = true) {
  if (state.currentNote?.id !== id && !confirmDiscardEdit()) return;
  try {
    const [noteResult, contextResult] = await Promise.all([
      api(`/api/notes/${id}`),
      api(`/api/notes/${id}/context?count=2`).catch(() => null),
    ]);
    state.currentNote = noteResult.note;
    state.noteContext = contextResult;
    state.editing = false;
    $("#libraryView").hidden = true;
    $("#noteView").hidden = false;
    renderNoteDetail();
    window.scrollTo({ top: 0, behavior: "smooth" });
    if (pushHistory) history.pushState({ noteId: id }, "", `/?note=${id}`);
  } catch (error) {
    showToast("没有找到这条记录。");
  }
}

function renderNoteDetail() {
  const note = state.currentNote;
  if (!note) return;
  const created = new Date(note.createdAt);
  $("#noteCreatedAt").textContent = new Intl.DateTimeFormat("zh-CN", { dateStyle: "long", timeStyle: "short" }).format(created);
  $("#noteCreatedAt").dateTime = note.createdAt;
  $("#noteTitleInput").value = note.title || "";
  $("#noteTitleInput").readOnly = !state.editing;
  $("#noteRendered").textContent = note.content;
  $("#noteRendered").hidden = state.editing;
  $("#noteEditor").value = note.content;
  $("#noteEditor").hidden = !state.editing;
  $("#editActions").hidden = !state.editing;
  $("#editNoteButton").hidden = state.editing || Boolean(note.deletedAt);
  $("#starNoteButton").hidden = state.editing || Boolean(note.deletedAt);
  $("#starNoteButton").textContent = note.starred ? "★ 已星标" : "☆ 星标";
  $("#deleteNoteButton").hidden = state.editing;
  $("#deleteNoteButton").classList.toggle("danger", !note.deletedAt);
  $("#deleteNoteButton").textContent = note.deletedAt ? "恢复这条记录" : "移到回收站";
  $("#noteSaveState").textContent = note.deletedAt ? "在回收站中" : `第 ${note.revision} 版 · 已保存`;
  $("#continueNoteButton").hidden = state.editing || Boolean(note.deletedAt);
  renderContext();
}

function renderContext() {
  const container = $("#contextTimeline");
  container.replaceChildren();
  const context = state.noteContext;
  const current = state.currentNote;
  const notes = context
    ? [...(context.before || []).slice().reverse(), context.current, ...(context.after || [])]
    : [current];
  for (const note of notes.filter(Boolean)) {
    const item = document.createElement("div");
    item.className = `context-item${note.id === current.id ? " current" : ""}`;
    const button = document.createElement("button");
    button.type = "button";
    const time = document.createElement("time");
    time.textContent = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(new Date(note.createdAt));
    const excerpt = document.createElement("span");
    excerpt.textContent = note.title || note.content;
    button.append(time, excerpt);
    if (note.id !== current.id) button.addEventListener("click", () => openNote(note.id));
    item.append(button);
    container.append(item);
  }
}

function setEditing(editing) {
  state.editing = editing;
  renderNoteDetail();
  if (editing) {
    $("#noteEditor").focus();
    $("#noteEditor").setSelectionRange($("#noteEditor").value.length, $("#noteEditor").value.length);
  }
}

function hasUnsavedEdit() {
  const note = state.currentNote;
  return Boolean(state.editing && note && (
    $("#noteEditor").value !== note.content || $("#noteTitleInput").value !== note.title
  ));
}

function confirmDiscardEdit() {
  return !hasUnsavedEdit() || confirm("这条记录还有未保存的修改。确定离开并放弃它们吗？");
}

async function saveEdit() {
  const note = state.currentNote;
  const content = $("#noteEditor").value;
  const title = $("#noteTitleInput").value;
  if (!content.trim()) {
    $("#noteSaveState").textContent = "正文不能为空；原内容尚未改变";
    return;
  }
  $("#saveEditButton").disabled = true;
  $("#noteSaveState").textContent = "正在保存…";
  try {
    const result = await api(`/api/notes/${note.id}`, {
      method: "PATCH",
      body: JSON.stringify({ title, content, expectedRevision: note.revision }),
    });
    state.currentNote = result.note;
    state.editing = false;
    renderNoteDetail();
    showToast("修改已经保存。");
    loadNotes();
  } catch (error) {
    if (error.status === 409) {
      $("#noteSaveState").textContent = "另一处已经修改；你的文字仍保留在编辑器中";
      showToast("检测到版本冲突，没有覆盖任何内容。");
    } else {
      $("#noteSaveState").textContent = "未能保存；修改仍保留在编辑器中";
      showToast("修改没有保存，请重试。");
    }
  } finally {
    $("#saveEditButton").disabled = false;
  }
}

async function deleteOrRestoreCurrent() {
  const note = state.currentNote;
  if (note.deletedAt) {
    await restoreNote(note.id);
    showLibrary();
    return;
  }
  if (!confirm("把这条记录移到回收站？之后仍然可以恢复。")) return;
  try {
    await api(`/api/notes/${note.id}`, { method: "DELETE" });
    showToast("已移到回收站。 ");
    showLibrary();
    loadNotes();
  } catch {
    showToast("没有移除，请重试。");
  }
}

async function restoreNote(id) {
  try {
    await api(`/api/notes/${id}/restore`, { method: "POST", body: "{}" });
    showToast("已经恢复。 ");
    await loadNotes();
  } catch {
    showToast("没有恢复，请重试。");
  }
}

function continueFromCurrent() {
  state.continuedFromId = state.currentNote.id;
  ensureFreshRequestForChangedDraft();
  updateContinuationChip();
  persistDraft();
  showLibrary();
  requestAnimationFrame(() => {
    $("#captureInput").placeholder = "继续写…";
    $("#captureInput").focus();
  });
}

function updateContinuationChip() {
  $("#continuationChip").hidden = !state.continuedFromId;
  if (!state.continuedFromId) $("#captureInput").placeholder = "写下想法…";
}

function showLibrary(pushHistory = true, discardConfirmed = false) {
  if (!discardConfirmed && !confirmDiscardEdit()) return false;
  state.editing = false;
  $("#noteView").hidden = true;
  $("#libraryView").hidden = false;
  if (pushHistory) history.pushState({}, "", "/");
  return true;
}

function selectView(view) {
  if (!confirmDiscardEdit()) return false;
  state.view = view;
  state.query = "";
  $("#searchInput").value = "";
  $("#clearSearchButton").hidden = true;
  $$(".nav-item").forEach((button) => button.classList.toggle("active", button.dataset.view === view));
  showLibrary(true, true);
  loadNotes();
  return true;
}

function focusCapture() {
  if (state.view === "trash") {
    if (!selectView("recent")) return;
  } else if (!showLibrary()) return;
  requestAnimationFrame(() => {
    $("#captureInput").scrollIntoView({ behavior: "smooth", block: "center" });
    $("#captureInput").focus();
  });
}

let searchTimer;
function handleSearch() {
  state.query = $("#searchInput").value;
  $("#clearSearchButton").hidden = !state.query;
  clearTimeout(searchTimer);
  searchTimer = setTimeout(loadNotes, 180);
}

function bindEvents() {
  $("#captureForm").addEventListener("submit", submitCapture);
  let composing = false;
  $("#captureInput").addEventListener("compositionstart", () => { composing = true; });
  $("#captureInput").addEventListener("compositionend", () => { composing = false; });
  $("#captureInput").addEventListener("input", () => {
    ensureFreshRequestForChangedDraft();
    $("#captureError").textContent = "";
    resizeCapture();
    persistDraft();
  });
  $("#captureInput").addEventListener("keydown", (event) => {
    if (!composing && event.key === "Enter" && (event.metaKey || event.ctrlKey)) submitCapture(event);
  });
  $("#searchInput").addEventListener("input", handleSearch);
  $("#clearSearchButton").addEventListener("click", () => {
    $("#searchInput").value = "";
    state.query = "";
    $("#clearSearchButton").hidden = true;
    loadNotes();
    $("#searchInput").focus();
  });
  $$(".nav-item").forEach((button) => button.addEventListener("click", () => selectView(button.dataset.view)));
  $("#brandButton").addEventListener("click", () => selectView("recent"));
  $("#newNoteButton").addEventListener("click", focusCapture);
  $("#backButton").addEventListener("click", () => {
    if (!confirmDiscardEdit()) return;
    state.editing = false;
    if (history.state?.noteId) history.back();
    else showLibrary(true, true);
  });
  $("#editNoteButton").addEventListener("click", () => setEditing(true));
  $("#cancelEditButton").addEventListener("click", () => setEditing(false));
  $("#saveEditButton").addEventListener("click", saveEdit);
  $("#noteEditor").addEventListener("keydown", (event) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) saveEdit();
  });
  $("#starNoteButton").addEventListener("click", () => toggleStar(state.currentNote));
  $("#deleteNoteButton").addEventListener("click", deleteOrRestoreCurrent);
  $("#continueNoteButton").addEventListener("click", continueFromCurrent);
  $("#clearContinuationButton").addEventListener("click", () => {
    state.continuedFromId = null;
    ensureFreshRequestForChangedDraft();
    updateContinuationChip();
    persistDraft();
  });
  window.addEventListener("keydown", (event) => {
    const target = event.target;
    const typing = target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement;
    if (event.key === "/" && !typing) {
      event.preventDefault();
      $("#searchInput").focus();
    }
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "n") {
      event.preventDefault();
      focusCapture();
    }
    if (event.key === "Escape" && document.activeElement === $("#searchInput") && state.query) {
      $("#clearSearchButton").click();
    }
  });
  window.addEventListener("popstate", (event) => {
    if (!confirmDiscardEdit()) {
      const currentID = state.currentNote?.id;
      history.pushState(currentID ? { noteId: currentID } : {}, "", currentID ? `/?note=${currentID}` : "/");
      return;
    }
    state.editing = false;
    const noteID = event.state?.noteId || Number(new URL(location.href).searchParams.get("note"));
    if (noteID) openNote(noteID, false);
    else showLibrary(false);
  });
  window.addEventListener("pagehide", () => persistDraft(true));
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") persistDraft(true);
  });
}

async function init() {
  bindEvents();
  restoreDraft();
  await loadNotes();
  const noteID = Number(new URL(location.href).searchParams.get("note"));
  if (noteID) openNote(noteID, false);
}

init();
