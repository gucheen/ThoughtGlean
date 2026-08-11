import { db, newID, queue, readDeviceMetadata, selectLibrary, type Attachment, type SyncPayload, upsertIncoming, writeDeviceMetadata } from "./db";

const encoder = new TextEncoder();
const decoder = new TextDecoder();
const configKey = "sync.config";
const cursorKey = (vaultId: string) => `sync.cursor.${vaultId}`;

const toB64 = (bytes: Uint8Array) => btoa(String.fromCharCode(...bytes)).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
const fromB64 = (value: string) => Uint8Array.from(atob(value.replaceAll("-", "+").replaceAll("_", "/") + "=".repeat((4 - value.length % 4) % 4)), char => char.charCodeAt(0));

export type SyncConfig = { relayURL: string; vaultID: string };
export type PairingCode = SyncConfig & { recoveryCode: string };

export const pairingCode = ({ vaultID, recoveryCode }: Pick<PairingCode, "vaultID" | "recoveryCode">) => `tg1.${vaultID}.${recoveryCode}`;
export function parsePairingCode(value: string): Pick<PairingCode, "vaultID" | "recoveryCode"> {
  const match = value.trim().match(/^tg1\.([A-Za-z0-9_-]{22,128})\.([A-Za-z0-9_-]{43})$/);
  if (!match) throw new Error("配对码格式不正确");
  return { vaultID: match[1], recoveryCode: match[2] };
}

export class VaultSession {
  private readonly relayURL: string;
  private cipherKey: CryptoKey | null;
  private token: Uint8Array | null;
  private readonly enrollmentToken?: string;
  private readonly controller = new AbortController();
  readonly vaultID: string;

  private constructor(relayURL: string, vaultID: string, cipherKey: CryptoKey, token: Uint8Array, enrollmentToken?: string) {
	this.relayURL = new URL(relayURL).toString().replace(/\/$/, ""); this.vaultID = vaultID; this.cipherKey = cipherKey; this.token = token; this.enrollmentToken = enrollmentToken;
  }

  static createVault() { return { vaultID: newID(), recoveryCode: newID(32) }; }

  static async unlock(config: SyncConfig & { recoveryCode: string; enrollmentToken?: string }) {
    if (!crypto.subtle || !/^[A-Za-z0-9_-]{22,128}$/.test(config.vaultID)) throw new Error("同步库标识格式不正确");
    const recovery = fromB64(config.recoveryCode);
    if (recovery.byteLength !== 32) throw new Error("恢复代码格式不正确");
    const material = await crypto.subtle.importKey("raw", recovery, "HKDF", false, ["deriveBits"]);
    const bits = new Uint8Array(await crypto.subtle.deriveBits({ name: "HKDF", hash: "SHA-256", salt: encoder.encode(config.vaultID), info: encoder.encode("ThoughtGlean encrypted sync v1") }, material, 512));
    const cipherKey = await crypto.subtle.importKey("raw", bits.slice(0, 32), "AES-GCM", false, ["encrypt", "decrypt"]);
    return new VaultSession(config.relayURL, config.vaultID, cipherKey, bits.slice(32), config.enrollmentToken);
  }

  private headers(json = false) {
    if (!this.token) throw new Error("同步库已锁定");
    return { Authorization: `Bearer ${toB64(this.token)}`, "X-ThoughtGlean-Vault": this.vaultID, ...(json ? { "Content-Type": "application/json" } : {}) };
  }
  private async encrypt(value: unknown) { return this.encryptBytes(encoder.encode(JSON.stringify(value)), true) as Promise<string>; }
  private async encryptBytes(value: Uint8Array, encoded = false): Promise<Uint8Array | string> {
    if (!this.cipherKey) throw new Error("同步库已锁定");
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const encrypted = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv, additionalData: encoder.encode(this.vaultID) }, this.cipherKey, value as unknown as BufferSource));
    const envelope = encoder.encode(JSON.stringify({ v: 1, iv: toB64(iv), data: toB64(encrypted) }));
    return encoded ? toB64(envelope) : envelope;
  }
  private async decryptBytes(value: Uint8Array) {
    if (!this.cipherKey) throw new Error("同步库已锁定");
    const envelope = JSON.parse(decoder.decode(value)) as { v: number; iv: string; data: string };
    return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: fromB64(envelope.iv), additionalData: encoder.encode(this.vaultID) }, this.cipherKey, fromB64(envelope.data)));
  }
  private async decrypt(value: string) { return JSON.parse(decoder.decode(await this.decryptBytes(fromB64(value)))) as SyncPayload; }

  async claim() { const r = await fetch(`${this.relayURL}/api/sync/v1/vaults`, { method: "POST", headers: { ...this.headers(), ...(this.enrollmentToken ? { "X-ThoughtGlean-Enrollment": this.enrollmentToken } : {}) }, signal: this.controller.signal }); if (!r.ok) throw new Error("无法连接或验证加密同步库"); }
  async uploadBlob(id: string, blob: Blob) { const body = await this.encryptBytes(new Uint8Array(await blob.arrayBuffer())) as Uint8Array; const r = await fetch(`${this.relayURL}/api/sync/v1/blobs/${encodeURIComponent(id)}`, { method: "PUT", headers: this.headers(), body: body as unknown as BodyInit }); if (!r.ok) throw new Error("加密图片上传失败"); }
  async downloadBlob(id: string) { const r = await fetch(`${this.relayURL}/api/sync/v1/blobs/${encodeURIComponent(id)}`, { headers: this.headers() }); if (!r.ok) throw new Error("加密图片下载失败"); return new Blob([await this.decryptBytes(new Uint8Array(await r.arrayBuffer()))]); }

  async sync() {
    const cursor = Number((await db.metadata.get(cursorKey(this.vaultID)))?.value ?? 0);
    const pulled = await fetch(`${this.relayURL}/api/sync/v1/operations?after=${cursor}`, { headers: this.headers() });
    if (!pulled.ok) throw new Error("加密同步下载失败");
    const result = await pulled.json() as { nextCursor: number; operations: { ciphertext: string }[] };
    for (const operation of result.operations) {
      const payload = await this.decrypt(operation.ciphertext);
      const blob = payload.kind === "attachment.upsert" ? await this.downloadBlob(payload.blobId) : undefined;
      await upsertIncoming(payload, blob);
    }
    await db.metadata.put({ key: cursorKey(this.vaultID), value: result.nextCursor });
    const events = await db.events.orderBy("createdAt").toArray();
    for (const event of events) {
      if (event.payload.kind === "attachment.upsert") {
        const item = await db.attachments.get(event.payload.attachment.id);
        if (item) await this.uploadBlob(event.payload.blobId, item.blob);
      }
      const encrypted = await this.encrypt(event.payload);
      const push = await fetch(`${this.relayURL}/api/sync/v1/operations`, { method: "POST", headers: this.headers(true), body: JSON.stringify({ operations: [{ operationId: event.id, ciphertext: encrypted, createdAt: event.createdAt }] }) });
      if (!push.ok) throw new Error("加密同步上传失败");
      await db.events.delete(event.id);
    }
    return events.length;
  }

  async waitForChanges() {
    const cursor = Number((await db.metadata.get(cursorKey(this.vaultID)))?.value ?? 0);
    const response = await fetch(`${this.relayURL}/api/sync/v1/subscribe?after=${encodeURIComponent(cursor)}`, { headers: this.headers(), signal: this.controller.signal });
    if (!response.ok) throw new Error("实时同步订阅失败");
  }

  lock() { this.controller.abort(); this.token?.fill(0); this.token = null; this.cipherKey = null; }
}

export async function savedSyncConfig() { return readDeviceMetadata<SyncConfig>(configKey); }
export async function saveSyncConfig(config: SyncConfig) { await writeDeviceMetadata(configKey, config); }
export async function useSyncLibrary(config: SyncConfig) { await selectLibrary(config.vaultID); }
export async function queueAttachment(noteSyncId: string, attachment: Attachment) { await queue({ kind: "attachment.upsert", noteSyncId, attachment: { ...attachment, blob: undefined } as Omit<Attachment, "blob">, blobId: attachment.syncId }); }
