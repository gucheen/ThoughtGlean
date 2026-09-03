import Dexie, { type EntityTable } from "dexie";

export type NoteKind = "note" | "procedure" | "material";

export type Note = {
  id: string;
  syncId: string;
  title: string;
  content: string;
  kind?: NoteKind;
  starred: boolean;
  continuedFromId?: string;
  derivedFromId?: string;
  revision: number;
  createdAt: string;
  updatedAt: string;
  deletedAt?: string;
};

export type NoteSource = { noteId: string; url: string; title: string; updatedAt: string };

export type NoteMaterialLink = { id: string; syncId: string; noteId: string; materialId: string; createdAt: string };
export type Topic = { id: string; syncId: string; name: string; createdAt: string; updatedAt: string; deletedAt?: string };
export type TopicMembership = { id: string; syncId: string; topicId: string; noteId: string; pinned: boolean; createdAt: string; updatedAt: string; deletedAt?: string };
export type VerificationResult = "success" | "partial" | "failed";
export type NoteVerification = {
  id: string;
  syncId: string;
  noteId: string;
  noteRevision: number;
  verifiedAt: string;
  environment: string;
  result: VerificationResult;
  comment: string;
};

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
  | { kind: "note.upsert"; note: Note; continuedFromSyncId?: string; derivedFromSyncId?: string }
  | { kind: "source.upsert"; noteSyncId: string; source: NoteSource }
  | { kind: "material-link.upsert"; noteSyncId: string; materialSyncId: string; materialLink: NoteMaterialLink }
  | { kind: "verification.upsert"; noteSyncId: string; verification: NoteVerification }
  | { kind: "topic.upsert"; topic: Topic }
  | { kind: "topic-membership.upsert"; topicSyncId: string; noteSyncId: string; membership: TopicMembership }
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
  materialLinks!: EntityTable<NoteMaterialLink, "id">;
  verifications!: EntityTable<NoteVerification, "id">;
  topics!: EntityTable<Topic, "id">;
  topicMemberships!: EntityTable<TopicMembership, "id">;

  constructor(name: string) {
    super(name);
    this.version(1).stores({
      notes: "id, syncId, updatedAt, deletedAt, starred, continuedFromId",
      sources: "noteId, updatedAt",
      attachments: "id, syncId, noteId, createdAt",
      events: "id, createdAt",
      metadata: "key",
    });
    this.version(2).stores({
      notes: "id, syncId, updatedAt, deletedAt, starred, kind, continuedFromId, derivedFromId",
      sources: "noteId, updatedAt",
      attachments: "id, syncId, noteId, createdAt",
      events: "id, createdAt",
      metadata: "key",
    }).upgrade(transaction => transaction.table<Note, string>("notes").toCollection().modify(note => { note.kind ??= "note"; }));
    this.version(3).stores({
      notes: "id, syncId, updatedAt, deletedAt, starred, kind, continuedFromId, derivedFromId",
      sources: "noteId, updatedAt",
      attachments: "id, syncId, noteId, createdAt",
      events: "id, createdAt",
      metadata: "key",
      materialLinks: "id, syncId, noteId, materialId, [noteId+materialId], createdAt",
      verifications: "id, syncId, noteId, [noteId+noteRevision], verifiedAt, result",
    }).upgrade(async transaction => {
      const notes = await transaction.table<Note, string>("notes").toArray();
      const links = notes.filter(note => note.kind === "procedure" && note.derivedFromId).map(note => ({
        id: newID(), syncId: newID(), noteId: note.id, materialId: note.derivedFromId!, createdAt: note.createdAt,
      }));
      if (links.length) await transaction.table<NoteMaterialLink, string>("materialLinks").bulkPut(links);
    });
    this.version(4).stores({
      notes: "id, syncId, updatedAt, deletedAt, starred, kind, continuedFromId, derivedFromId",
      sources: "noteId, updatedAt",
      attachments: "id, syncId, noteId, createdAt",
      events: "id, createdAt",
      metadata: "key",
      materialLinks: "id, syncId, noteId, materialId, [noteId+materialId], createdAt",
      verifications: "id, syncId, noteId, [noteId+noteRevision], verifiedAt, result",
      topics: "id, syncId, name, updatedAt, deletedAt",
      topicMemberships: "id, syncId, topicId, noteId, [topicId+noteId], pinned, updatedAt, deletedAt",
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
  const [notes, sources, attachments, events, metadata, materialLinks, verifications, topics, topicMemberships] = await Promise.all([from.notes.toArray(), from.sources.toArray(), from.attachments.toArray(), from.events.toArray(), from.metadata.toArray(), from.materialLinks.toArray(), from.verifications.toArray(), from.topics.toArray(), from.topicMemberships.toArray()]);
  if (!notes.length && !sources.length && !attachments.length && !events.length && !materialLinks.length && !verifications.length && !topics.length && !topicMemberships.length) return;
  await to.transaction("rw", [to.notes, to.sources, to.attachments, to.events, to.metadata, to.materialLinks, to.verifications, to.topics, to.topicMemberships], async () => {
    await Promise.all([to.notes.bulkPut(notes), to.sources.bulkPut(sources), to.attachments.bulkPut(attachments), to.events.bulkPut(events), to.metadata.bulkPut(metadata), to.materialLinks.bulkPut(materialLinks), to.verifications.bulkPut(verifications), to.topics.bulkPut(topics), to.topicMemberships.bulkPut(topicMemberships)]);
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
    const sourceMaterial = note.derivedFromId ? await db.notes.get(note.derivedFromId) : undefined;
    await db.notes.put(note);
    await db.events.put({
      id: newID(),
      payload: { kind: "note.upsert", note, continuedFromSyncId: parent?.syncId, derivedFromSyncId: sourceMaterial?.syncId },
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

export async function createNote(content: string, continuedFromId?: string, options: { kind?: NoteKind; derivedFromId?: string } = {}) {
  const kind = options.kind ?? "note";
  if (options.derivedFromId && kind !== "procedure") throw new Error("只有操作记录可以关联原始素材");
  if (options.derivedFromId) {
    const sourceMaterial = await db.notes.get(options.derivedFromId);
    if (!sourceMaterial || sourceMaterial.kind !== "material" || sourceMaterial.deletedAt) throw new Error("关联的原始素材不可用");
  }
  const timestamp = now();
  const fields = fieldsFromQuickCapture(content);
  const note: Note = {
    id: newID(), syncId: newID(), title: fields.title,
    content: fields.content, kind, starred: false, continuedFromId, derivedFromId: options.derivedFromId, revision: 1, createdAt: timestamp, updatedAt: timestamp,
  };
  await saveNote(note);
  if (options.derivedFromId) {
    const material = await db.notes.get(options.derivedFromId);
    if (material) await saveMaterialLink(note, material);
  }
  return note;
}

export async function saveMaterialLink(note: Note, material: Note) {
  if (note.kind !== "procedure" || material.kind !== "material" || note.id === material.id) throw new Error("来源关系必须连接操作记录和原始素材");
  const existing = await db.materialLinks.where("[noteId+materialId]").equals([note.id, material.id]).first();
  if (existing) return existing;
  const link: NoteMaterialLink = { id: newID(), syncId: newID(), noteId: note.id, materialId: material.id, createdAt: now() };
  await db.transaction("rw", db.materialLinks, db.events, async () => {
    await db.materialLinks.put(link);
    await db.events.put({ id: newID(), payload: { kind: "material-link.upsert", noteSyncId: note.syncId, materialSyncId: material.syncId, materialLink: link }, createdAt: now() });
  });
  return link;
}

export async function recordVerification(note: Note, input: { environment: string; result: VerificationResult; comment?: string }) {
  if (note.kind !== "procedure" || !input.environment.trim()) throw new Error("请填写本次使用环境");
  const verification: NoteVerification = {
    id: newID(), syncId: newID(), noteId: note.id, noteRevision: note.revision, verifiedAt: now(),
    environment: input.environment.trim(), result: input.result, comment: input.comment?.trim() ?? "",
  };
  await db.transaction("rw", db.verifications, db.events, async () => {
    await db.verifications.put(verification);
    await db.events.put({ id: newID(), payload: { kind: "verification.upsert", noteSyncId: note.syncId, verification }, createdAt: now() });
  });
  return verification;
}

export async function createTopic(name: string) {
  const value = name.trim();
  if (!value || value.length > 80) throw new Error("主题名称应为 1–80 个字符");
  const duplicate = await db.topics.filter(topic => !topic.deletedAt && topic.name.toLocaleLowerCase() === value.toLocaleLowerCase()).first();
  if (duplicate) throw new Error("已经存在同名主题");
  const timestamp = now();
  const topic: Topic = { id: newID(), syncId: newID(), name: value, createdAt: timestamp, updatedAt: timestamp };
  await saveTopic(topic);
  return topic;
}

export async function saveTopic(topic: Topic, shouldQueue = true) {
  const name = topic.name.trim();
  if ((!topic.deletedAt && !name) || name.length > 80) throw new Error("主题名称应为 1–80 个字符");
  const next = { ...topic, name };
  if (!shouldQueue) { await db.topics.put(next); return next; }
  await db.transaction("rw", db.topics, db.events, async () => {
    await db.topics.put(next);
    const superseded = await db.events.filter(event => event.payload.kind === "topic.upsert" && event.payload.topic.syncId === next.syncId).primaryKeys();
    if (superseded.length) await db.events.bulkDelete(superseded);
    await db.events.put({ id: newID(), payload: { kind: "topic.upsert", topic: next }, createdAt: now() });
  });
  return next;
}

export async function saveTopicMembership(topic: Topic, note: Note, input: { pinned?: boolean; deletedAt?: string } = {}) {
  if (!input.deletedAt && (topic.deletedAt || note.deletedAt)) throw new Error("主题或记录已不可用");
  if (!input.deletedAt && input.pinned && note.kind !== "procedure") throw new Error("只有操作记录可以在主题内置顶");
  const timestamp = now();
  const existing = await db.topicMemberships.where("[topicId+noteId]").equals([topic.id, note.id]).first();
  const membership: TopicMembership = existing ? {
    ...existing, pinned: input.pinned ?? existing.pinned, deletedAt: input.deletedAt, updatedAt: timestamp,
  } : {
    id: newID(), syncId: newID(), topicId: topic.id, noteId: note.id, pinned: input.pinned ?? false,
    createdAt: timestamp, updatedAt: timestamp, deletedAt: input.deletedAt,
  };
  await db.transaction("rw", db.topicMemberships, db.events, async () => {
    await db.topicMemberships.put(membership);
    const superseded = await db.events.filter(event => event.payload.kind === "topic-membership.upsert" && event.payload.membership.syncId === membership.syncId).primaryKeys();
    if (superseded.length) await db.events.bulkDelete(superseded);
    await db.events.put({ id: newID(), payload: { kind: "topic-membership.upsert", topicSyncId: topic.syncId, noteSyncId: note.syncId, membership }, createdAt: timestamp });
  });
  return membership;
}

export async function upsertIncoming(payload: SyncPayload, blob?: Blob, authoritative = false) {
  if (payload.kind === "note.upsert") {
    const existing = await db.notes.where("syncId").equals(payload.note.syncId).first();
    if (!existing || authoritative || payload.note.updatedAt > existing.updatedAt || (payload.note.updatedAt === existing.updatedAt && payload.note.revision > existing.revision)) {
      const parent = payload.continuedFromSyncId ? await db.notes.where("syncId").equals(payload.continuedFromSyncId).first() : undefined;
      const sourceMaterial = payload.derivedFromSyncId ? await db.notes.where("syncId").equals(payload.derivedFromSyncId).first() : undefined;
      await saveNote({ ...payload.note, kind: payload.note.kind ?? "note", id: existing?.id ?? payload.note.id, continuedFromId: parent?.id, derivedFromId: sourceMaterial?.id }, false);
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
  if (payload.kind === "material-link.upsert") {
    const note = await db.notes.where("syncId").equals(payload.noteSyncId).first();
    const material = await db.notes.where("syncId").equals(payload.materialSyncId).first();
    if (note && material) {
      const existing = await db.materialLinks.where("syncId").equals(payload.materialLink.syncId).first();
      const pair = await db.materialLinks.where("[noteId+materialId]").equals([note.id, material.id]).first();
      if (!existing) {
        if (pair) await db.materialLinks.delete(pair.id);
        await db.materialLinks.put({ ...payload.materialLink, id: payload.materialLink.id, noteId: note.id, materialId: material.id });
      }
    }
    return;
  }
  if (payload.kind === "verification.upsert") {
    const note = await db.notes.where("syncId").equals(payload.noteSyncId).first();
    const existing = await db.verifications.where("syncId").equals(payload.verification.syncId).first();
    if (note && !existing) await db.verifications.put({ ...payload.verification, id: payload.verification.id, noteId: note.id });
    return;
  }
  if (payload.kind === "topic.upsert") {
    const existing = await db.topics.where("syncId").equals(payload.topic.syncId).first();
    await saveTopic({ ...payload.topic, id: existing?.id ?? payload.topic.id }, false);
    return;
  }
  if (payload.kind === "topic-membership.upsert") {
    const topic = await db.topics.where("syncId").equals(payload.topicSyncId).first();
    const note = await db.notes.where("syncId").equals(payload.noteSyncId).first();
    if (topic && note) {
      const existing = await db.topicMemberships.where("syncId").equals(payload.membership.syncId).first();
      const pair = await db.topicMemberships.where("[topicId+noteId]").equals([topic.id, note.id]).first();
      if (pair && pair.id !== existing?.id) await db.topicMemberships.delete(pair.id);
      await db.topicMemberships.put({ ...payload.membership, id: existing?.id ?? payload.membership.id, topicId: topic.id, noteId: note.id });
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
  materialLinks?: NoteMaterialLink[];
  verifications?: NoteVerification[];
  topics?: Topic[];
  topicMemberships?: TopicMembership[];
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
  const [notes, sources, materialLinks, verifications, topics, topicMemberships, attachments] = await Promise.all([db.notes.toArray(), db.sources.toArray(), db.materialLinks.toArray(), db.verifications.toArray(), db.topics.toArray(), db.topicMemberships.toArray(), db.attachments.toArray()]);
  return {
    format: "thoughtglean-browser-backup", version: 1, generatedAt: now(), notes, sources, materialLinks, verifications, topics, topicMemberships,
    attachments: await Promise.all(attachments.map(async ({ blob, ...attachment }) => ({ ...attachment, data: await blobToBase64(blob) }))),
  };
}

export async function restoreBackup(value: unknown) {
  const backup = value as Partial<BrowserBackup>;
  if (backup.format !== "thoughtglean-browser-backup" || backup.version !== 1 || !Array.isArray(backup.notes) || !Array.isArray(backup.sources) || !Array.isArray(backup.attachments)) throw new Error("不是可恢复的拾念浏览器备份");
  const notes = backup.notes as Note[];
  const sources = backup.sources as NoteSource[];
  const materialLinks = Array.isArray(backup.materialLinks) ? backup.materialLinks as NoteMaterialLink[] : [];
  const verifications = Array.isArray(backup.verifications) ? backup.verifications as NoteVerification[] : [];
  const topics = Array.isArray(backup.topics) ? backup.topics as Topic[] : [];
  const topicMemberships = Array.isArray(backup.topicMemberships) ? backup.topicMemberships as TopicMembership[] : [];
  const attachments = backup.attachments as BrowserBackup["attachments"];
  if (!notes.every(note => typeof note.id === "string" && typeof note.syncId === "string" && typeof note.content === "string")) throw new Error("备份中的笔记格式无效");
  const noteByID = new Map(notes.map(note => [note.id, note]));
  if (!materialLinks.every(link => typeof link.id === "string" && typeof link.syncId === "string" && noteByID.get(link.noteId)?.kind === "procedure" && noteByID.get(link.materialId)?.kind === "material")) throw new Error("备份中的素材来源关系无效");
  if (!verifications.every(item => typeof item.id === "string" && typeof item.syncId === "string" && noteByID.get(item.noteId)?.kind === "procedure" && item.noteRevision > 0 && item.noteRevision <= (noteByID.get(item.noteId)?.revision ?? 0) && ["success", "partial", "failed"].includes(item.result) && typeof item.environment === "string" && Boolean(item.environment.trim()))) throw new Error("备份中的使用记录无效");
  const topicByID = new Map(topics.map(topic => [topic.id, topic]));
  if (!topics.every(topic => typeof topic.id === "string" && typeof topic.syncId === "string" && typeof topic.name === "string" && Boolean(topic.name.trim()))) throw new Error("备份中的主题无效");
  if (!topicMemberships.every(item => typeof item.id === "string" && typeof item.syncId === "string" && topicByID.has(item.topicId) && noteByID.has(item.noteId) && typeof item.pinned === "boolean" && (!item.pinned || noteByID.get(item.noteId)?.kind === "procedure"))) throw new Error("备份中的主题成员关系无效");
  await db.transaction("rw", [db.notes, db.sources, db.materialLinks, db.verifications, db.topics, db.topicMemberships, db.attachments, db.events], async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.materialLinks.clear(), db.verifications.clear(), db.topics.clear(), db.topicMemberships.clear(), db.attachments.clear(), db.events.clear()]);
    await db.notes.bulkPut(notes.map(note => ({ ...note, kind: note.kind ?? "note" })));
    await db.sources.bulkPut(sources);
    await db.materialLinks.bulkPut(materialLinks);
    await db.verifications.bulkPut(verifications);
    await db.topics.bulkPut(topics);
    await db.topicMemberships.bulkPut(topicMemberships);
    await db.attachments.bulkPut(attachments.map(({ data, ...attachment }) => ({ ...attachment, blob: base64ToBlob(data, attachment.mimeType) })));
  });
}

export async function queueLibrarySnapshot() {
  const [notes, sources, materialLinks, verifications, topics, topicMemberships, attachments] = await Promise.all([db.notes.toArray(), db.sources.toArray(), db.materialLinks.toArray(), db.verifications.toArray(), db.topics.toArray(), db.topicMemberships.toArray(), db.attachments.toArray()]);
  const noteByID = new Map(notes.map(note => [note.id, note]));
  for (const topic of topics) await queue({ kind: "topic.upsert", topic });
  for (const note of notes) {
    await queue({ kind: "note.upsert", note, continuedFromSyncId: note.continuedFromId ? noteByID.get(note.continuedFromId)?.syncId : undefined, derivedFromSyncId: note.derivedFromId ? noteByID.get(note.derivedFromId)?.syncId : undefined });
  }
  for (const source of sources) {
    const note = noteByID.get(source.noteId);
    if (note) await queue({ kind: "source.upsert", noteSyncId: note.syncId, source });
  }
  for (const link of materialLinks) {
    const note = noteByID.get(link.noteId); const material = noteByID.get(link.materialId);
    if (note && material) await queue({ kind: "material-link.upsert", noteSyncId: note.syncId, materialSyncId: material.syncId, materialLink: link });
  }
  for (const verification of verifications) {
    const note = noteByID.get(verification.noteId);
    if (note) await queue({ kind: "verification.upsert", noteSyncId: note.syncId, verification });
  }
  for (const membership of topicMemberships) {
    const topic = topics.find(item => item.id === membership.topicId); const note = noteByID.get(membership.noteId);
    if (topic && note) await queue({ kind: "topic-membership.upsert", topicSyncId: topic.syncId, noteSyncId: note.syncId, membership });
  }
  for (const attachment of attachments) {
    const note = noteByID.get(attachment.noteId);
    if (note) await queue({ kind: "attachment.upsert", noteSyncId: note.syncId, attachment: { ...attachment, blob: undefined } as Omit<Attachment, "blob">, blobId: attachment.syncId });
  }
}

export async function markdownExport() {
  const [allNotes, sources, materialLinks, verifications, topics, topicMemberships] = await Promise.all([db.notes.toArray(), db.sources.toArray(), db.materialLinks.toArray(), db.verifications.toArray(), db.topics.toArray(), db.topicMemberships.toArray()]);
  const notes = allNotes.filter(note => !note.deletedAt).sort((left, right) => left.createdAt.localeCompare(right.createdAt) || left.id.localeCompare(right.id));
  const sourceByNote = new Map(sources.map(source => [source.noteId, source]));
  const noteByID = new Map(allNotes.map(note => [note.id, note]));
  const topicByID = new Map(topics.filter(topic => !topic.deletedAt).map(topic => [topic.id, topic]));
  return notes.map(note => {
    const source = sourceByNote.get(note.id);
    const materialIDs = new Set(materialLinks.filter(link => link.noteId === note.id).map(link => link.materialId));
    if (note.derivedFromId) materialIDs.add(note.derivedFromId);
    const materialTitles = [...materialIDs].map(id => noteByID.get(id)?.title).filter((title): title is string => Boolean(title));
    const noteVerifications = verifications.filter(item => item.noteId === note.id);
    const topicNames = topicMemberships.filter(item => item.noteId === note.id && !item.deletedAt).map(item => topicByID.get(item.topicId)?.name).filter((name): name is string => Boolean(name)).sort();
    const kind = note.kind === "procedure" ? `类型：操作记录\n状态：${verificationExportStatus(note, noteVerifications)}` : note.kind === "material" ? "类型：原始素材" : "";
    return [`# ${note.title}`, "", `创建于：${note.createdAt}`, topicNames.length ? `主题：${topicNames.join("、")}` : "", kind, materialTitles.length ? `提炼自：${materialTitles.join("、")}` : "", source ? `来源：${source.title || source.url} (${source.url})` : "", "", note.content, ""].filter(Boolean).join("\n");
  }).join("\n---\n\n");
}

function verificationExportStatus(note: Note, verifications: NoteVerification[]) {
  const current = verifications.filter(item => item.noteRevision === note.revision).sort((left, right) => right.verifiedAt.localeCompare(left.verifiedAt) || right.id.localeCompare(left.id))[0];
  if (current?.result === "success") return `已验证 · ${current.environment}`;
  if (current?.result === "partial") return "最近使用部分成功";
  if (current?.result === "failed") return "最近使用失败";
  return verifications.length ? "当前版本待重新验证" : "未实际验证";
}
