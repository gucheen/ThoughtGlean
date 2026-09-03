import { beforeEach, describe, expect, it } from "vitest";
import { createNote, createTopic, db, exportBackup, newID, now, readLibraryMetadata, recordVerification, restoreBackup, saveAttachment, saveTopicMembership, writeLibraryMetadata, type Attachment } from "./db";

beforeEach(async () => {
  await db.open();
  await db.transaction("rw", [db.notes, db.sources, db.materialLinks, db.verifications, db.topics, db.topicMemberships, db.attachments, db.events, db.metadata], async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.materialLinks.clear(), db.verifications.clear(), db.topics.clear(), db.topicMemberships.clear(), db.attachments.clear(), db.events.clear(), db.metadata.clear()]);
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

  it("persists source material and queues a derived operation with the stable source identity", async () => {
    const material = await createNote("# Docker 对话\n\n原始内容", undefined, { kind: "material" });
    await db.events.clear();
    const procedure = await createNote("# 查看 Docker 占用\n\n状态：未实际验证", undefined, { kind: "procedure", derivedFromId: material.id });
    expect(await db.notes.get(procedure.id)).toMatchObject({ kind: "procedure", derivedFromId: material.id });
    const events = await db.events.toArray();
    expect(events.find(event => event.payload.kind === "note.upsert")?.payload).toMatchObject({
      kind: "note.upsert", note: { id: procedure.id, kind: "procedure" }, derivedFromSyncId: material.syncId,
    });
    expect(events.find(event => event.payload.kind === "material-link.upsert")?.payload).toMatchObject({
      kind: "material-link.upsert", noteSyncId: procedure.syncId, materialSyncId: material.syncId,
    });
    await expect(db.materialLinks.where("[noteId+materialId]").equals([procedure.id, material.id]).count()).resolves.toBe(1);
    await expect(createNote("错误类型", undefined, { derivedFromId: material.id })).rejects.toThrow("只有操作记录");
  });

  it("records immutable use results against the current procedure revision", async () => {
    const procedure = await createNote("# 检查服务\n\nsystemctl status nginx", undefined, { kind: "procedure" });
    await db.events.clear();
    const verification = await recordVerification(procedure, { environment: "Ubuntu 24.04 / nginx 1.26", result: "success", comment: "服务正常" });
    expect(verification).toMatchObject({ noteId: procedure.id, noteRevision: 1, result: "success" });
    expect((await db.events.toArray())[0].payload).toMatchObject({ kind: "verification.upsert", noteSyncId: procedure.syncId });
  });

  it("restores material links and revision-bound verification history", async () => {
    const material = await createNote("# 来源\n\n对话", undefined, { kind: "material" });
    const procedure = await createNote("# 检查服务\n\nsystemctl status nginx", undefined, { kind: "procedure", derivedFromId: material.id });
    await recordVerification(procedure, { environment: "Debian 13", result: "partial", comment: "输出不同" });
    const backup = await exportBackup();
    await db.materialLinks.clear(); await db.verifications.clear();

    await restoreBackup(backup);

    await expect(db.materialLinks.count()).resolves.toBe(1);
    await expect(db.verifications.where("[noteId+noteRevision]").equals([procedure.id, 1]).count()).resolves.toBe(1);
  });

  it("stores lightweight topics, membership state, and topic-local pinning in backups", async () => {
    const procedure = await createNote("# 重启服务\n\nsystemctl restart nginx", undefined, { kind: "procedure" });
    const topic = await createTopic("服务器管理");
    const membership = await saveTopicMembership(topic, procedure);
    const pinned = await saveTopicMembership(topic, procedure, { pinned: true });
    expect(pinned).toMatchObject({ id: membership.id, topicId: topic.id, noteId: procedure.id, pinned: true });
    const membershipEvents = (await db.events.toArray()).filter(event => event.payload.kind === "topic-membership.upsert");
    expect(membershipEvents).toHaveLength(1);
    expect(membershipEvents[0].payload).toMatchObject({ membership: { pinned: true } });

    const backup = await exportBackup();
    await db.topics.clear(); await db.topicMemberships.clear();
    await restoreBackup(backup);

    await expect(db.topics.get(topic.id)).resolves.toMatchObject({ name: "服务器管理" });
    await expect(db.topicMemberships.get(membership.id)).resolves.toMatchObject({ pinned: true });
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
