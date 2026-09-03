import { beforeEach, describe, expect, it, vi } from "vitest";
import { createNote, createTopic, db, newID, now, recordVerification, saveAttachment, saveTopicMembership, type Attachment } from "./db";
import { ServerSync } from "./sync";

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), {
  status,
  headers: { "Content-Type": "application/json" },
});

beforeEach(async () => {
  await db.open();
  await db.transaction("rw", [db.notes, db.sources, db.materialLinks, db.verifications, db.topics, db.topicMemberships, db.attachments, db.events, db.metadata], async () => {
    await Promise.all([db.notes.clear(), db.sources.clear(), db.materialLinks.clear(), db.verifications.clear(), db.topics.clear(), db.topicMemberships.clear(), db.attachments.clear(), db.events.clear(), db.metadata.clear()]);
  });
});

describe("server sync", () => {
  it("uploads a queued image and clears its event after the server accepts it", async () => {
    class TestFormData {
      private values = new Map<string, FormDataEntryValue>();
      set(name: string, value: FormDataEntryValue) { this.values.set(name, value); }
      get(name: string) { return this.values.get(name) ?? null; }
    }
    vi.stubGlobal("FormData", TestFormData);
    const note = await createNote("图片同步");
    await db.events.clear();
    const attachment: Attachment = {
      id: newID(), syncId: newID(), noteId: note.id, originalName: "photo.png", altText: "现场照片",
      mimeType: "image/png", byteSize: 3, createdAt: now(), blob: new Blob(["png"], { type: "image/png" }),
    };
    await saveAttachment(note.syncId, attachment);

    let upload: FormData | undefined;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === "string" ? input : input.toString();
      if (path === "/api/sync/attachments") {
        upload = init?.body as FormData;
        return json({}, 201);
      }
      if (path === "/api/sync/snapshot") {
        return json({ generatedAt: now(), notes: [note], sources: [], attachments: [{ ...attachment, blob: undefined }] });
      }
      return json({});
    }));

    await expect(new ServerSync().sync()).resolves.toBe(1);
    expect(upload?.get("noteSyncId")).toBe(note.syncId);
    expect(upload?.get("syncId")).toBe(attachment.syncId);
    expect(upload?.get("altText")).toBe("现场照片");
    expect(upload?.get("image")).toBeTruthy();
    await expect(db.events.count()).resolves.toBe(0);
  });

  it("uploads topics, notes, relations, and verification events in dependency order", async () => {
    const material = await createNote("# Docker 对话\n\n原始内容", undefined, { kind: "material" });
    const procedure = await createNote("# 查看 Docker 占用\n\n```bash\ndocker system df -v\n```", undefined, { kind: "procedure", derivedFromId: material.id });
    await recordVerification(procedure, { environment: "Ubuntu 24.04", result: "success" });
    const topic = await createTopic("服务器管理");
    await saveTopicMembership(topic, procedure, { pinned: true });
    const pushed: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === "string" ? input : input.toString();
      if (path === "/api/sync/apply") {
        pushed.push((JSON.parse(init?.body as string) as { kind: string }).kind);
        return json({});
      }
      if (path === "/api/sync/snapshot") return json({
        generatedAt: now(), notes: [material, procedure], sources: [], materialLinks: await db.materialLinks.toArray(), verifications: await db.verifications.toArray(), topics: [topic], topicMemberships: await db.topicMemberships.toArray(), attachments: [],
      });
      return json({});
    }));

    await expect(new ServerSync().sync()).resolves.toBe(6);
    expect(pushed).toEqual(["topic.upsert", "note.upsert", "note.upsert", "material-link.upsert", "verification.upsert", "topic-membership.upsert"]);
  });
});
