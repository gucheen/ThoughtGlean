import { db, newID, queue, type Attachment, type Note, type NoteSource, type SyncPayload, upsertIncoming } from "./db";

type ServerAttachment = Omit<Attachment, "blob"> & { contentHash?: string };
type Snapshot = { generatedAt: string; notes: Note[]; sources: NoteSource[]; attachments: ServerAttachment[] };

export type AuthStatus = { enabled: boolean; configured: boolean; authenticated: boolean; tokenLoginEnabled: boolean };
export type PasskeyInfo = { id: string; createdAt: string; updatedAt: string };
type CeremonyOptions = { ceremonyId: string; publicKey: Record<string, unknown> };

async function api(path: string, init?: RequestInit) {
  const response = await fetch(path, init);
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    throw new Error(body.error || (response.status === 401 ? "需要登录" : `服务器请求失败（${response.status}）`));
  }
  return response;
}

export async function authStatus(): Promise<AuthStatus> {
  const response = await fetch("/api/auth/status");
  if (!response.ok) throw new Error("无法连接服务器");
  return response.json() as Promise<AuthStatus>;
}

export async function login(token: string) {
  await api("/api/auth/token", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token }) });
}

export async function logout() {
  await api("/api/auth/logout", { method: "POST" });
}

const decodeBase64URL = (value: string) => {
  const normalized = value.replaceAll("-", "+").replaceAll("_", "/") + "=".repeat((4 - value.length % 4) % 4);
  return Uint8Array.from(atob(normalized), character => character.charCodeAt(0));
};

const encodeBase64URL = (value: ArrayBuffer) => btoa(String.fromCharCode(...new Uint8Array(value))).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");

function creationOptions(value: Record<string, unknown>): PublicKeyCredentialCreationOptions {
  const user = value.user as Record<string, unknown>;
  const excludeCredentials = value.excludeCredentials as Array<Record<string, unknown>> | undefined;
  return {
    ...value,
    challenge: decodeBase64URL(value.challenge as string),
    user: { ...user, id: decodeBase64URL(user.id as string) },
    excludeCredentials: excludeCredentials?.map(item => ({ ...item, id: decodeBase64URL(item.id as string) })),
  } as unknown as PublicKeyCredentialCreationOptions;
}

function requestOptions(value: Record<string, unknown>): PublicKeyCredentialRequestOptions {
  const allowCredentials = value.allowCredentials as Array<Record<string, unknown>> | undefined;
  return {
    ...value,
    challenge: decodeBase64URL(value.challenge as string),
    allowCredentials: allowCredentials?.map(item => ({ ...item, id: decodeBase64URL(item.id as string) })),
  } as unknown as PublicKeyCredentialRequestOptions;
}

function credentialJSON(credential: PublicKeyCredential) {
  const response = credential.response;
  if (response instanceof AuthenticatorAttestationResponse) {
    return {
      id: credential.id, rawId: encodeBase64URL(credential.rawId), type: credential.type,
      clientExtensionResults: credential.getClientExtensionResults(),
      response: {
        clientDataJSON: encodeBase64URL(response.clientDataJSON),
        attestationObject: encodeBase64URL(response.attestationObject),
        transports: response.getTransports?.() ?? [],
      },
    };
  }
  const assertion = response as AuthenticatorAssertionResponse;
  return {
    id: credential.id, rawId: encodeBase64URL(credential.rawId), type: credential.type,
    clientExtensionResults: credential.getClientExtensionResults(),
    response: {
      clientDataJSON: encodeBase64URL(assertion.clientDataJSON),
      authenticatorData: encodeBase64URL(assertion.authenticatorData),
      signature: encodeBase64URL(assertion.signature),
      userHandle: assertion.userHandle ? encodeBase64URL(assertion.userHandle) : null,
    },
  };
}

function passkeyError(error: unknown): Error {
  if (error instanceof DOMException && error.name === "NotAllowedError") return new Error("Passkey 操作已取消或超时");
  if (error instanceof Error) return error;
  return new Error("Passkey 操作失败");
}

export async function registerPasskey(additional: boolean) {
  if (!window.PublicKeyCredential || !navigator.credentials) throw new Error("当前浏览器不支持 Passkey");
  try {
    const optionsResponse = await api(additional ? "/api/auth/passkeys/options" : "/api/auth/register/options", additional ? { method: "POST" } : { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: "ThoughtGlean owner" }) });
    const options = await optionsResponse.json() as CeremonyOptions;
    const credential = await navigator.credentials.create({ publicKey: creationOptions(options.publicKey) }) as PublicKeyCredential | null;
    if (!credential) throw new Error("没有创建 Passkey");
    await api(additional ? "/api/auth/passkeys/verify" : "/api/auth/register/verify", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ceremonyId: options.ceremonyId, credential: credentialJSON(credential) }) });
  } catch (error) { throw passkeyError(error); }
}

export async function loginWithPasskey() {
  if (!window.PublicKeyCredential || !navigator.credentials) throw new Error("当前浏览器不支持 Passkey");
  try {
    const optionsResponse = await api("/api/auth/login/options", { method: "POST" });
    const options = await optionsResponse.json() as CeremonyOptions;
    const credential = await navigator.credentials.get({ publicKey: requestOptions(options.publicKey) }) as PublicKeyCredential | null;
    if (!credential) throw new Error("没有取得 Passkey");
    await api("/api/auth/login/verify", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ceremonyId: options.ceremonyId, credential: credentialJSON(credential) }) });
  } catch (error) { throw passkeyError(error); }
}

export async function listPasskeys(): Promise<PasskeyInfo[]> {
  const response = await api("/api/auth/passkeys");
  return ((await response.json()) as { passkeys: PasskeyInfo[] }).passkeys;
}

export async function deletePasskey(id: string) {
  await api(`/api/auth/passkeys/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export class ServerSync {
  async sync() {
    const events = await db.events.orderBy("createdAt").toArray();
    for (const event of events) {
      await this.push(event.payload);
      await db.events.delete(event.id);
    }

    const response = await api("/api/sync/snapshot");
    const snapshot = await response.json() as Snapshot;
    const notes = [...snapshot.notes].sort((left, right) => left.createdAt.localeCompare(right.createdAt));
    for (const note of notes) {
      const parent = note.continuedFromId ? snapshot.notes.find(item => item.id === note.continuedFromId) : undefined;
      await upsertIncoming({ kind: "note.upsert", note, continuedFromSyncId: parent?.syncId });
    }

    const noteByServerID = new Map(snapshot.notes.map(note => [note.id, note]));
    const serverNoteSyncIDs = new Set(snapshot.notes.map(note => note.syncId));
    const serverSourceNoteSyncIDs = new Set<string>();
    for (const source of snapshot.sources) {
      const note = noteByServerID.get(source.noteId);
      if (note) {
        serverSourceNoteSyncIDs.add(note.syncId);
        await upsertIncoming({ kind: "source.upsert", noteSyncId: note.syncId, source });
      }
    }
    for (const localSource of await db.sources.toArray()) {
      const localNote = await db.notes.get(localSource.noteId);
      if (localNote && serverNoteSyncIDs.has(localNote.syncId) && !serverSourceNoteSyncIDs.has(localNote.syncId)) await db.sources.delete(localSource.noteId);
    }
    const serverAttachmentSyncIDs = new Set(snapshot.attachments.map(item => item.syncId));
    for (const item of snapshot.attachments) {
      if (await db.attachments.where("syncId").equals(item.syncId).first()) continue;
      const note = noteByServerID.get(item.noteId);
      if (!note) continue;
      const image = await api(`/api/attachments/${encodeURIComponent(item.id)}`);
      await upsertIncoming({
        kind: "attachment.upsert",
        noteSyncId: note.syncId,
        attachment: { id: newID(), syncId: item.syncId, noteId: "", originalName: item.originalName, mimeType: item.mimeType, byteSize: item.byteSize, createdAt: item.createdAt },
        blobId: item.id,
      }, await image.blob());
    }
    for (const localAttachment of await db.attachments.toArray()) {
      const localNote = await db.notes.get(localAttachment.noteId);
      if (localNote && serverNoteSyncIDs.has(localNote.syncId) && !serverAttachmentSyncIDs.has(localAttachment.syncId)) await db.attachments.delete(localAttachment.id);
    }
    return events.length;
  }

  private async push(payload: SyncPayload) {
    if (payload.kind === "attachment.upsert") {
      const item = await db.attachments.where("syncId").equals(payload.attachment.syncId).first();
      if (!item) return;
      const form = new FormData();
      form.set("noteSyncId", payload.noteSyncId);
      form.set("syncId", item.syncId);
      form.set("image", item.blob, item.originalName);
      await api("/api/sync/attachments", { method: "POST", body: form });
      return;
    }
    await api("/api/sync/apply", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
  }
}

export async function queueAttachment(noteSyncId: string, attachment: Attachment) {
  await queue({ kind: "attachment.upsert", noteSyncId, attachment: { ...attachment, blob: undefined } as Omit<Attachment, "blob">, blobId: attachment.syncId });
}
