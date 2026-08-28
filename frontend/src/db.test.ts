import { beforeEach, describe, expect, it } from "vitest";
import { createNote, db, newID, now, readLibraryMetadata, saveAttachment, writeLibraryMetadata, type Attachment } from "./db";

beforeEach(async () => {
  await db.open();
  await db.transaction("rw", db.notes, db.sources, db.attachments, db.events, db.metadata, async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.attachments.clear(), db.events.clear(), db.metadata.clear()]);
  });
});

describe("local persistence", () => {
  it("saves a quick note and its sync event atomically", async () => {
    const note = await createNote("# 标题\n\n正文");
    expect(await db.notes.get(note.id)).toMatchObject({ title: "标题", content: "正文" });
    const events = await db.events.toArray();
    expect(events).toHaveLength(1);
    expect(events[0].payload).toMatchObject({ kind: "note.upsert", note: { id: note.id } });
  });

  it("restores a persisted home draft", async () => {
    await writeLibraryMetadata("draft.home.v1", { content: "没有丢失的草稿", sourceURL: "https://example.com" });
    await expect(readLibraryMetadata("draft.home.v1")).resolves.toEqual({ content: "没有丢失的草稿", sourceURL: "https://example.com" });
  });

  it("persists a continuation and queues its parent sync identity", async () => {
    const parent = await createNote("最初的想法");
    await db.events.clear();
    const continuation = await createNote("接着想下去", parent.id);
    expect(await db.notes.get(continuation.id)).toMatchObject({ continuedFromId: parent.id });
    expect((await db.events.toArray())[0].payload).toMatchObject({
      kind: "note.upsert", note: { id: continuation.id, continuedFromId: parent.id }, continuedFromSyncId: parent.syncId,
    });
  });

  it("queues an image for sync with its local blob", async () => {
    const note = await createNote("图片记录");
    await db.events.clear();
    const attachment: Attachment = { id: newID(), syncId: newID(), noteId: note.id, originalName: "photo.png", altText: "现场照片", mimeType: "image/png", byteSize: 3, createdAt: now(), blob: new Blob(["png"], { type: "image/png" }) };
    await saveAttachment(note.syncId, attachment);
    expect(await db.attachments.get(attachment.id)).toMatchObject({ altText: "现场照片" });
    expect((await db.events.toArray())[0].payload).toMatchObject({ kind: "attachment.upsert", noteSyncId: note.syncId });
  });
});
