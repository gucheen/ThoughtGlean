import { useEffect, useRef, useState } from "react";

export type PendingShare = {
  id: string;
  title: string;
  text: string;
  url: string;
  createdAt: string;
  materialNoteId?: string;
};

export type ProcedureCandidate = { id: string; title: string; content: string; updatedAt: string; status: string };

type Props = {
  item: PendingShare;
  remaining: number;
  procedures: ProcedureCandidate[];
  onClose: () => void;
  onDiscard: (item: PendingShare) => void;
  onMoveToCapture: (item: PendingShare) => void;
  onSaveMaterial: (item: PendingShare) => Promise<void>;
  onSaveProcedure: (item: PendingShare, content: string, retainMaterial: boolean, finish: boolean, targetNoteId?: string) => Promise<void>;
};

function comparisonTerms(value: string) {
  const title = value.match(/^#\s+(.+)$/m)?.[1] ?? value.split(/\r?\n/, 1)[0] ?? "";
  const words = title.toLocaleLowerCase().normalize("NFKC").match(/[\p{Script=Han}]{2,}|[a-z0-9][a-z0-9._/-]*/gu) ?? [];
  return new Set(words.flatMap(word => /\p{Script=Han}/u.test(word) && word.length > 2 ? Array.from({ length: word.length - 1 }, (_, index) => word.slice(index, index + 2)) : [word]));
}

function codeBlocks(value: string) {
  return [...value.matchAll(/```[^\n]*\n([\s\S]*?)```/g)].map(match => match[1].trim()).filter(Boolean);
}

export function similarProcedureCandidates(draft: string, procedures: ProcedureCandidate[]) {
  const draftTerms = comparisonTerms(draft);
  const draftCode = codeBlocks(draft);
  return procedures.map(candidate => {
    const candidateTerms = comparisonTerms(`# ${candidate.title}`);
    const overlap = [...draftTerms].filter(term => candidateTerms.has(term)).length;
    const union = new Set([...draftTerms, ...candidateTerms]).size || 1;
    const codeMatch = draftCode.some(block => codeBlocks(candidate.content).some(existing => existing === block));
    return { candidate, score: (overlap / union) + (codeMatch ? 1 : 0) };
  }).filter(item => item.score >= 0.25).sort((left, right) => right.score - left.score || right.candidate.updatedAt.localeCompare(left.candidate.updatedAt)).slice(0, 3).map(item => item.candidate);
}

export function procedureTemplate(title: string) {
  const value = title.trim().replace(/^#+\s*/, "").slice(0, 80) || "待命名操作";
  return `# ${value}\n\n## 什么时候使用\n\n\n## 适用环境与前置条件\n\n未确认。\n\n## 操作\n\n\n## 如何验证\n\n\n## 风险与回退\n\n未确认。\n\n## 搜索关键词\n\n`;
}

export function sharedMaterialText(item: Pick<PendingShare, "title" | "text" | "url">) {
  const title = item.title.trim() || "原始分享材料";
  const body = [item.text.trim(), item.url.trim()].filter((part, index, values) => part && values.indexOf(part) === index).join("\n\n");
  return `# ${title}\n\n${body || "未提供文字内容。"}`;
}

export function ShareReview({ item, remaining, procedures, onClose, onDiscard, onMoveToCapture, onSaveMaterial, onSaveProcedure }: Props) {
  const [mode, setMode] = useState<"review" | "procedure">("review");
  const [procedure, setProcedure] = useState(() => procedureTemplate(item.title));
  const [retainMaterial, setRetainMaterial] = useState(() => !item.url || Boolean(item.materialNoteId));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [targetNoteId, setTargetNoteId] = useState<string>();
  const source = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    setMode("review");
    setProcedure(procedureTemplate(item.title));
    setRetainMaterial(!item.url || Boolean(item.materialNoteId));
    setTargetNoteId(undefined);
    setError("");
  }, [item.id]);

  const run = async (action: () => Promise<void>) => {
    if (busy) return;
    setBusy(true); setError("");
    try { await action(); }
    catch (reason) { setError(reason instanceof Error ? reason.message : "操作失败，原始分享仍已保留。"); }
    finally { setBusy(false); }
  };

  const addSelection = () => {
    const element = source.current;
    if (!element) return;
    const selected = element.value.slice(element.selectionStart, element.selectionEnd).trim();
    if (!selected) { setError("请先在左侧原始内容中选择需要保留的文字或命令。"); return; }
    setProcedure(current => `${current.trimEnd()}\n\n${selected}\n`);
    setError("");
  };

  const effectiveRetainMaterial = retainMaterial || Boolean(item.materialNoteId) || Boolean(targetNoteId);
  const target = targetNoteId ? procedures.find(candidate => candidate.id === targetNoteId) : undefined;
  const similar = similarProcedureCandidates(procedure, procedures).filter(candidate => candidate.id !== targetNoteId);
  return <div className="modal-backdrop share-review-backdrop" role="presentation">
    <section className={`dialog-card share-review-dialog ${mode === "procedure" ? "is-distilling" : ""}`} role="dialog" aria-modal="true" aria-label="整理分享内容">
      <header><div><p className="eyebrow">分享内容</p><h2>{mode === "procedure" ? "整理为操作记录" : "先决定怎样保存"}</h2><p className="muted">{remaining > 1 ? `还有 ${remaining - 1} 条分享内容将在之后显示。` : "原始内容会保留到你明确处理为止。"}</p></div><button className="text-button" disabled={busy} onClick={onClose}>稍后处理</button></header>
      {mode === "review" ? <div className="share-review-summary">
        <div><strong>{item.title || "未命名分享"}</strong><span>{item.text.length.toLocaleString()} 字{item.url ? " · 包含来源链接" : ""}</span></div>
        <pre>{item.text || item.url || "没有文字内容"}</pre>
        <p className="context-copy">长对话适合先整理为可独立查找的操作记录。保存为原始素材后，它不会进入普通记录列表。</p>
        {error && <p className="inline-error" role="alert">{error}</p>}
        <div className="share-review-actions">
          <button className="button button-ghost" disabled={busy} onClick={() => { if (confirm("忽略这条分享内容？尚未保存的文字将被移除。")) onDiscard(item); }}>忽略</button>
          <button className="button button-secondary" disabled={busy} onClick={() => onMoveToCapture(item)}>放入快速记录</button>
          <button className="button button-secondary" disabled={busy} onClick={() => void run(() => onSaveMaterial(item))}>{busy ? "保存中…" : "保存为原始素材"}</button>
          <button className="button button-primary" disabled={busy} onClick={() => setMode("procedure")}>整理为操作记录</button>
        </div>
      </div> : <div className="distillation-workspace">
        <section className="distillation-source"><header><div><strong>原始内容</strong><small>{item.title || item.url || "分享文本"}</small></div><button className="button button-secondary" type="button" onClick={addSelection}>将选中内容加入草稿</button></header><textarea ref={source} readOnly aria-label="原始分享内容" value={item.text || item.url} /></section>
        <section className="distillation-draft"><header><div><strong>操作记录草稿</strong><small>命令和技术原文请保持原样</small></div></header><textarea aria-label="操作记录草稿" value={procedure} onChange={event => setProcedure(event.target.value)} />
          {target && <aside className="similar-update-target"><span><strong>将更新已有记录</strong><small>{target.title} · 保存后产生新版本，并保留本次素材来源</small></span><button className="text-button" type="button" onClick={() => setTargetNoteId(undefined)}>改为新建</button><details><summary>查看当前保存的版本</summary><pre>{`# ${target.title}\n\n${target.content}`}</pre><button className="button button-secondary" type="button" onClick={() => { if (confirm("用已有版本替换当前草稿？当前尚未保存的整理内容会被覆盖。")) setProcedure(`# ${target.title}\n\n${target.content}`); }}>载入已有版本作为底稿</button></details></aside>}
          {!target && similar.length > 0 && <aside className="similar-procedures" aria-label="可能重复的操作记录"><strong>可能已经存在相关记录</strong>{similar.map(candidate => <div key={candidate.id}><span><b>{candidate.title}</b><small>{candidate.status} · 更新于 {new Date(candidate.updatedAt).toLocaleDateString()}</small></span><button className="button button-secondary" type="button" onClick={() => setTargetNoteId(candidate.id)}>查看并更新</button></div>)}</aside>}
        </section>
        <footer className="distillation-footer">
          <label><input type="checkbox" checked={effectiveRetainMaterial} disabled={Boolean(item.materialNoteId) || Boolean(target)} onChange={event => setRetainMaterial(event.target.checked)} /> 保留完整原始内容作为素材{target ? "（更新已有记录时用于追加来源）" : item.url ? "（可取消，仅保留来源链接）" : ""}</label>
          {error && <p className="inline-error" role="alert">{error}</p>}
          <div><button className="button button-ghost" disabled={busy} onClick={() => setMode("review")}>返回</button><button className="button button-secondary" disabled={busy || !procedure.trim()} onClick={() => void run(async () => { await onSaveProcedure(item, procedure, effectiveRetainMaterial, false, targetNoteId); setProcedure(procedureTemplate(item.title)); setTargetNoteId(undefined); })}>{target ? "更新并继续提炼" : "保存并继续提炼"}</button><button className="button button-primary" disabled={busy || !procedure.trim()} onClick={() => void run(() => onSaveProcedure(item, procedure, effectiveRetainMaterial, true, targetNoteId))}>{busy ? "保存中…" : target ? "更新并完成" : "保存并完成"}</button></div>
        </footer>
      </div>}
    </section>
  </div>;
}
