import { useState } from "react";

export type PendingShare = {
  id: string;
  title: string;
  text: string;
  url: string;
  createdAt: string;
  materialNoteId?: string;
};

export function sharedNoteText(item: PendingShare) {
  return [item.title ? `# ${item.title}` : "", item.text, item.url && !item.text.includes(item.url) ? item.url : ""].filter(Boolean).join("\n\n");
}

export function ShareReview({ item, onClose, onDiscard, onMoveToCapture, onSave }: {
  item: PendingShare;
  onClose: () => void;
  onDiscard: (item: PendingShare) => Promise<void>;
  onMoveToCapture: (item: PendingShare) => Promise<void>;
  onSave: (item: PendingShare) => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const run = async (action: () => Promise<void>) => {
    if (busy) return;
    setBusy(true); setError("");
    try { await action(); }
    catch { setError("保存失败，分享内容仍然保留，请重试。"); }
    finally { setBusy(false); }
  };
  return <div className="modal-backdrop share-review-backdrop">
    <section className="dialog-card share-review-dialog" role="dialog" aria-modal="true" aria-label="收到的分享">
      <header><h2>收到的分享</h2><button className="text-button" disabled={busy} onClick={onClose}>稍后</button></header>
      <div className="share-review-summary">
        <pre>{sharedNoteText(item)}</pre>
        {error && <p className="inline-error" role="alert">{error}</p>}
        <div className="share-review-actions">
          <button className="button button-ghost" disabled={busy} onClick={() => { if (confirm("丢弃这条尚未保存的分享？")) void run(() => onDiscard(item)); }}>丢弃</button>
          <button className="button button-secondary" disabled={busy} onClick={() => void run(() => onMoveToCapture(item))}>加入草稿</button>
          <button className="button button-primary" disabled={busy} onClick={() => void run(() => onSave(item))}>{busy ? "保存中…" : "保存记录"}</button>
        </div>
      </div>
    </section>
  </div>;
}
