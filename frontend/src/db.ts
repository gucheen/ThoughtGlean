import Dexie, { type EntityTable } from "dexie";

export type Note = {
  id: string;
  syncId: string;
  title: string;
  content: string;
  starred: boolean;
  continuedFromId?: string;
  revision: number;
  createdAt: string;
  updatedAt: string;
  deletedAt?: string;
};

export type NoteSource = { noteId: string; url: string; title: string; updatedAt: string };

export type Attachment = {
  id: string;
  syncId: string;
  noteId: string;
  originalName: string;
  altText?: string;
  mimeType: string;
  byteSize: number;
  createdAt: string;
  blob: Blob;
};

export type SyncEvent = { id: string; payload: SyncPayload; createdAt: string };
export type SyncPayload =
  | { kind: "note.upsert"; note: Note; continuedFromSyncId?: string }
  | { kind: "source.upsert"; noteSyncId: string; source: NoteSource }
  | { kind: "attachment.upsert"; noteSyncId: string; attachment: Omit<Attachment, "blob">; blobId: string }
  | { kind: "attachment.update"; attachmentSyncId: string; altText: string }
  | { kind: "attachment.delete"; attachmentSyncId: string };

export type Metadata = { key: string; value: unknown };

class ThoughtGleanDB extends Dexie {
  notes!: EntityTable<Note, "id">;
  sources!: EntityTable<NoteSource, "noteId">;
  attachments!: EntityTable<Attachment, "id">;
  events!: EntityTable<SyncEvent, "id">;
  metadata!: EntityTable<Metadata, "key">;

  constructor(name: string) {
    super(name);
    this.version(1).stores({
      notes: "id, syncId, updatedAt, deletedAt, starred, continuedFromId",
      sources: "noteId, updatedAt",
      attachments: "id, syncId, noteId, createdAt",
      events: "id, createdAt",
      metadata: "key",
    });
  }
}

class DeviceDB extends Dexie {
  metadata!: EntityTable<Metadata, "key">;
  constructor() { super("thoughtglean-device-v1"); this.version(1).stores({ metadata: "key" }); }
}

const device = new DeviceDB();
export let db = new ThoughtGleanDB("thoughtglean-library-local");
export const libraryName = (id: string) => `thoughtglean-library-${id}`;

async function copyLibrary(from: ThoughtGleanDB, to: ThoughtGleanDB) {
  const [notes, sources, attachments, events, metadata] = await Promise.all([from.notes.toArray(), from.sources.toArray(), from.attachments.toArray(), from.events.toArray(), from.metadata.toArray()]);
  if (!notes.length && !sources.length && !attachments.length && !events.length) return;
  await to.transaction("rw", to.notes, to.sources, to.attachments, to.events, to.metadata, async () => {
    await Promise.all([to.notes.bulkPut(notes), to.sources.bulkPut(sources), to.attachments.bulkPut(attachments), to.events.bulkPut(events), to.metadata.bulkPut(metadata)]);
  });
}

export async function selectLibrary(id: string) {
  const target = new ThoughtGleanDB(libraryName(id));
  await target.open();
  if (await target.notes.count() === 0) await copyLibrary(db, target);
  db.close();
  db = target;
}

export async function migrateLegacyLibrary() {
  const legacy = new ThoughtGleanDB("thoughtglean-v1");
  if (!await Dexie.exists("thoughtglean-v1") || await db.notes.count() > 0) return;
  await legacy.open(); await copyLibrary(legacy, db); legacy.close();
}

export async function readDeviceMetadata<T>(key: string) { return (await device.metadata.get(key))?.value as T | undefined; }
export async function writeDeviceMetadata(key: string, value: unknown) { await device.metadata.put({ key, value }); }
export async function readLibraryMetadata<T>(key: string) { return (await db.metadata.get(key))?.value as T | undefined; }
export async function writeLibraryMetadata(key: string, value: unknown) { await db.metadata.put({ key, value }); }
export async function deleteLibraryMetadata(key: string) { await db.metadata.delete(key); }

export const newID = (bytes = 16) => {
  const value = crypto.getRandomValues(new Uint8Array(bytes));
  return btoa(String.fromCharCode(...value)).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
};

export const now = () => new Date().toISOString();

export async function queue(payload: SyncPayload) {
  await db.events.put({ id: newID(), payload, createdAt: now() });
}

export async function saveNote(note: Note, shouldQueue = true) {
  if (!shouldQueue) {
    await db.notes.put(note);
    return;
  }
  await db.transaction("rw", db.notes, db.events, async () => {
    const parent = note.continuedFromId ? await db.notes.get(note.continuedFromId) : undefined;
    await db.notes.put(note);
    await db.events.put({
      id: newID(),
      payload: { kind: "note.upsert", note, continuedFromSyncId: parent?.syncId },
      createdAt: now(),
    });
  });
}

export async function saveAttachment(noteSyncId: string, attachment: Attachment) {
  await db.transaction("rw", db.attachments, db.events, async () => {
    await db.attachments.put(attachment);
    await db.events.put({
      id: newID(),
      payload: {
        kind: "attachment.upsert",
        noteSyncId,
        attachment: { ...attachment, blob: undefined } as Omit<Attachment, "blob">,
        blobId: attachment.syncId,
      },
      createdAt: now(),
    });
  });
}

export async function deleteAttachment(attachment: Attachment) {
  await db.transaction("rw", db.attachments, db.events, async () => {
    await db.attachments.delete(attachment.id);
    await db.events.put({ id: newID(), payload: { kind: "attachment.delete", attachmentSyncId: attachment.syncId }, createdAt: now() });
  });
}

export async function updateAttachmentAlt(attachment: Attachment, altText: string) {
  const next = { ...attachment, altText };
  await db.transaction("rw", db.attachments, db.events, async () => {
    await db.attachments.put(next);
    await db.events.put({ id: newID(), payload: { kind: "attachment.update", attachmentSyncId: attachment.syncId, altText }, createdAt: now() });
  });
}

export function fieldsFromQuickCapture(value: string) {
  const lines = value.split(/\r?\n/);
  const firstContentLine = lines.findIndex(line => line.trim() !== "");
  const explicitTitle = firstContentLine >= 0 ? lines[firstContentLine].match(/^#\s+(.+?)\s*$/) : null;
  if (explicitTitle) {
    let bodyStart = firstContentLine + 1;
    while (bodyStart < lines.length && lines[bodyStart].trim() === "") bodyStart += 1;
    return { title: explicitTitle[1].trim().slice(0, 80), content: lines.slice(bodyStart).join("\n") };
  }
  return { title: value.trim().split(/\r?\n/, 1)[0].slice(0, 80) || "未命名记录", content: value };
}

export async function createNote(content: string, continuedFromId?: string) {
  const timestamp = now();
  const fields = fieldsFromQuickCapture(content);
  const note: Note = {
    id: newID(), syncId: newID(), title: fields.title,
    content: fields.content, starred: false, continuedFromId, revision: 1, createdAt: timestamp, updatedAt: timestamp,
  };
  await saveNote(note);
  return note;
}

export async function upsertIncoming(payload: SyncPayload, blob?: Blob, authoritative = false) {
  if (payload.kind === "note.upsert") {
    const existing = await db.notes.where("syncId").equals(payload.note.syncId).first();
    if (!existing || authoritative || payload.note.updatedAt > existing.updatedAt || (payload.note.updatedAt === existing.updatedAt && payload.note.revision > existing.revision)) {
      const parent = payload.continuedFromSyncId ? await db.notes.where("syncId").equals(payload.continuedFromSyncId).first() : undefined;
      await saveNote({ ...payload.note, id: existing?.id ?? payload.note.id, continuedFromId: parent?.id }, false);
    }
    return;
  }
  if (payload.kind === "source.upsert") {
    const note = await db.notes.where("syncId").equals(payload.noteSyncId).first();
    if (note) {
      if (payload.source.url) await db.sources.put({ ...payload.source, noteId: note.id });
      else await db.sources.delete(note.id);
    }
    return;
  }
  if (payload.kind === "attachment.delete") {
    const attachment = await db.attachments.where("syncId").equals(payload.attachmentSyncId).first();
    if (attachment) await db.attachments.delete(attachment.id);
    return;
  }
  if (payload.kind === "attachment.update") {
    const attachment = await db.attachments.where("syncId").equals(payload.attachmentSyncId).first();
    if (attachment) await db.attachments.put({ ...attachment, altText: payload.altText });
    return;
  }
  const note = await db.notes.where("syncId").equals(payload.noteSyncId).first();
  const existing = await db.attachments.where("syncId").equals(payload.attachment.syncId).first();
  if (note && blob && !existing) await db.attachments.put({ ...payload.attachment, id: payload.attachment.id, noteId: note.id, blob });
}

export type BrowserBackup = {
  format: "thoughtglean-browser-backup";
  version: 1;
  generatedAt: string;
  notes: Note[];
  sources: NoteSource[];
  attachments: Array<Omit<Attachment, "blob"> & { data: string }>;
};

const blobToBase64 = async (blob: Blob) => {
  const bytes = new Uint8Array(await blob.arrayBuffer());
  return btoa(String.fromCharCode(...bytes));
};

const base64ToBlob = (data: string, type: string) => {
  const bytes = Uint8Array.from(atob(data), (character) => character.charCodeAt(0));
  return new Blob([bytes], { type });
};

export async function exportBackup(): Promise<BrowserBackup> {
  const [notes, sources, attachments] = await Promise.all([db.notes.toArray(), db.sources.toArray(), db.attachments.toArray()]);
  return {
    format: "thoughtglean-browser-backup", version: 1, generatedAt: now(), notes, sources,
    attachments: await Promise.all(attachments.map(async ({ blob, ...attachment }) => ({ ...attachment, data: await blobToBase64(blob) }))),
  };
}

export async function restoreBackup(value: unknown) {
  const backup = value as Partial<BrowserBackup>;
  if (backup.format !== "thoughtglean-browser-backup" || backup.version !== 1 || !Array.isArray(backup.notes) || !Array.isArray(backup.sources) || !Array.isArray(backup.attachments)) throw new Error("不是可恢复的拾念浏览器备份");
  const notes = backup.notes as Note[];
  const sources = backup.sources as NoteSource[];
  const attachments = backup.attachments as BrowserBackup["attachments"];
  if (!notes.every(note => typeof note.id === "string" && typeof note.syncId === "string" && typeof note.content === "string")) throw new Error("备份中的笔记格式无效");
  await db.transaction("rw", db.notes, db.sources, db.attachments, db.events, async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.attachments.clear(), db.events.clear()]);
    await db.notes.bulkPut(notes);
    await db.sources.bulkPut(sources);
    await db.attachments.bulkPut(attachments.map(({ data, ...attachment }) => ({ ...attachment, blob: base64ToBlob(data, attachment.mimeType) })));
  });
}

export async function queueLibrarySnapshot() {
  const [notes, sources, attachments] = await Promise.all([db.notes.toArray(), db.sources.toArray(), db.attachments.toArray()]);
  const noteByID = new Map(notes.map(note => [note.id, note]));
  for (const note of notes) {
    await queue({ kind: "note.upsert", note, continuedFromSyncId: note.continuedFromId ? noteByID.get(note.continuedFromId)?.syncId : undefined });
  }
  for (const source of sources) {
    const note = noteByID.get(source.noteId);
    if (note) await queue({ kind: "source.upsert", noteSyncId: note.syncId, source });
  }
  for (const attachment of attachments) {
    const note = noteByID.get(attachment.noteId);
    if (note) await queue({ kind: "attachment.upsert", noteSyncId: note.syncId, attachment: { ...attachment, blob: undefined } as Omit<Attachment, "blob">, blobId: attachment.syncId });
  }
}

export async function markdownExport() {
  const [notes, sources] = await Promise.all([db.notes.filter(note => !note.deletedAt).sortBy("createdAt"), db.sources.toArray()]);
  const sourceByNote = new Map(sources.map(source => [source.noteId, source]));
  return notes.map(note => {
    const source = sourceByNote.get(note.id);
    return [`# ${note.title}`, "", `创建于：${note.createdAt}`, source ? `来源：${source.title || source.url} (${source.url})` : "", "", note.content, ""].filter(Boolean).join("\n");
  }).join("\n---\n\n");
}
