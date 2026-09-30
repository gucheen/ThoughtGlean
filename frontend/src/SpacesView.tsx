import { useEffect, useRef, useState, type ReactNode } from "react";
import { addNotesToSpace, createTopic, now, saveTopic, saveTopicMembership, type Note, type Topic, type TopicMembership } from "./db";

type Props = {
  topics: Topic[]; selectedTopic?: Topic; requestedTopicID?: string;
  notes: Note[]; allNotes: Note[]; memberships: TopicMembership[];
  query: string; keyboardSelectedID?: string; children: ReactNode;
  onOpenTopic: (id?: string) => void; onOpenNote: (id: string) => void; onChange: () => void;
};

export function SpacesView({ topics, selectedTopic, requestedTopicID, notes, allNotes, memberships, query, keyboardSelectedID, children, onOpenTopic, onOpenNote, onChange }: Props) {
  const [name, setName] = useState("");
  const [renaming, setRenaming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [candidateQuery, setCandidateQuery] = useState("");
  const [selection, setSelection] = useState<Set<string>>(new Set());
  const addButton = useRef<HTMLButtonElement>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (adding) dialog.current?.showModal();
    else if (dialog.current?.open) { dialog.current.close(); addButton.current?.focus(); }
  }, [adding]);

  const run = async (action: () => Promise<void>) => {
    if (busy) return;
    setBusy(true); setError("");
    try { await action(); onChange(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "保存失败，请重试"); }
    finally { setBusy(false); }
  };
  const patch = (value: Partial<Topic>) => run(async () => {
    if (selectedTopic) await saveTopic({ ...selectedTopic, ...value, updatedAt: now() });
  });
  const joined = memberships.filter(item => item.topicId === selectedTopic?.id);
  const joinedIDs = new Set(joined.map(item => item.noteId));
  const pinnedIDs = new Set(joined.filter(item => item.pinned).map(item => item.noteId));
  const candidateTerms = candidateQuery.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  const candidates = allNotes.filter(note => !note.deletedAt && !joinedIDs.has(note.id) && candidateTerms.every(term => `${note.title}\n${note.content}`.toLocaleLowerCase().includes(term)))
    .sort((left, right) => right.createdAt.localeCompare(left.createdAt));
  const terms = query.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  const listedTopics = topics.filter(topic => terms.every(term => topic.name.toLocaleLowerCase().includes(term)));
  const groups = [{ title: "置顶", items: notes.filter(note => pinnedIDs.has(note.id)) }, { title: "记录", items: notes.filter(note => !pinnedIDs.has(note.id)) }];
  const card = (topic: Topic) => <button key={topic.id} className="topic-card" onClick={() => onOpenTopic(topic.id)}><span><strong>{topic.name}</strong><small>{memberships.filter(item => item.topicId === topic.id).length} 条记录{topic.archivedAt ? " · 已归档" : topic.hiddenFromDaily ? " · 已从日常收起" : ""}</small></span><i aria-hidden="true">→</i></button>;

  if (requestedTopicID && !selectedTopic) return <section className="empty-state"><h1>空间暂不可用</h1><p>它可能尚未同步到本机，或已被删除。</p><button className="button button-secondary" onClick={() => onOpenTopic()}>返回空间列表</button></section>;

  return <section className="topics-view">
    {selectedTopic && <button className="text-button topic-overview-back" onClick={() => onOpenTopic()}>← 所有空间</button>}
    <header className="topics-heading"><div><h1>{selectedTopic?.name ?? "空间"}</h1><p>{selectedTopic ? selectedTopic.archivedAt ? "已归档，内容仍可阅读和搜索。恢复后可继续记录。" : "在这里记录和查找这个场景下需要的内容。" : "把一起使用的内容放在一起，需要时进入。"}</p></div>
      {selectedTopic && !selectedTopic.archivedAt && <div className="topic-heading-actions"><button ref={addButton} className="button button-secondary" onClick={() => { setSelection(new Set()); setCandidateQuery(""); setError(""); setAdding(true); }}>加入已有记录</button><button className="text-button" onClick={() => { setName(selectedTopic.name); setRenaming(value => !value); }}>重命名</button></div>}
    </header>
    {error && !adding && <p className="inline-error" role="alert">{error}</p>}
    {(!selectedTopic || renaming) && <form className="space-name-form" onSubmit={event => { event.preventDefault(); void run(async () => {
      if (selectedTopic) { await saveTopic({ ...selectedTopic, name, updatedAt: now() }); setRenaming(false); }
      else { const topic = await createTopic(name); onOpenTopic(topic.id); }
      setName("");
    }); }}><label>{selectedTopic ? "空间名称" : "新空间名称"}<input value={name} maxLength={80} onChange={event => setName(event.target.value)} placeholder="例如：最近在玩的游戏" /></label><button className="button button-secondary" disabled={busy || !name.trim()}>{selectedTopic ? "保存名称" : "创建空间"}</button></form>}
    {selectedTopic ? <>
      <div className="space-options">{selectedTopic.archivedAt ? <button className="button button-secondary" disabled={busy} onClick={() => void patch({ archivedAt: undefined })}>恢复空间</button> : <>
        <label><input type="checkbox" checked={Boolean(selectedTopic.hiddenFromDaily)} disabled={busy} onChange={event => void patch({ hiddenFromDaily: event.target.checked })} />从日常收起内容</label>
        <button className="text-button" disabled={busy} onClick={() => void patch({ archivedAt: now() })}>归档空间</button>
      </>}<small>{selectedTopic.archivedAt ? "归档内容不出现在日常，始终保留在全部记录中。" : "收起后仍可在此空间、星标和全部记录中找到。"}</small></div>
      {children}
      <div className="topic-groups">{groups.map(({ title, items }) => {
        return items.length > 0 && <section key={title} aria-label={title}><h2>{title}<small>{items.length}</small></h2><div className="topic-note-list">{items.map(note => <article key={note.id} className={`topic-note-card ${keyboardSelectedID === note.id ? "keyboard-selected" : ""}`} data-note-id={note.id}>
          <button className="topic-note-open" onClick={() => onOpenNote(note.id)}><strong>{note.title}</strong><p>{note.content.slice(0, 240)}</p></button>
          {!selectedTopic.archivedAt && <footer><button className="text-button" disabled={busy} onClick={() => void run(async () => { await saveTopicMembership(selectedTopic, note, { pinned: !pinnedIDs.has(note.id) }); })}>{pinnedIDs.has(note.id) ? "取消置顶" : "置顶"}</button><button className="text-button" disabled={busy} onClick={() => void run(async () => { await saveTopicMembership(selectedTopic, note, { pinned: false, deletedAt: now() }); })}>移出空间</button></footer>}
        </article>)}</div></section>;
      })}</div>
      {!notes.length && <section className="empty-state"><h2>{query ? "没有匹配的记录" : "空间里还没有记录"}</h2><p>{query ? "试试其他关键词，或切换到全部记录查找。" : "记下第一条内容，或加入已有记录。"}</p></section>}
    </> : <>
      <div className="topic-grid">{listedTopics.filter(topic => !topic.archivedAt).map(card)}</div>
      {!listedTopics.length && <p className="muted">{query ? "没有匹配的空间。" : "还没有使用中的空间。不需要聚合时，直接在日常记录就好。"}</p>}
      {listedTopics.some(topic => topic.archivedAt) && <details className="archived-spaces"><summary>已归档空间</summary><div className="topic-grid">{listedTopics.filter(topic => topic.archivedAt).map(card)}</div></details>}
    </>}
    <dialog ref={dialog} className="dialog-card space-add-dialog" aria-label="加入已有记录" onCancel={event => { event.preventDefault(); if (!busy) setAdding(false); }}>
      <header><h2>加入已有记录</h2><button className="text-button" disabled={busy} onClick={() => setAdding(false)}>关闭</button></header>
      <input aria-label="查找可加入的记录" type="search" value={candidateQuery} onChange={event => setCandidateQuery(event.target.value)} placeholder="搜索全部记录" />
      <div className="space-candidates">{candidates.map(note => <label key={note.id}><input type="checkbox" checked={selection.has(note.id)} disabled={busy} onChange={event => setSelection(current => { const next = new Set(current); if (event.target.checked) next.add(note.id); else next.delete(note.id); return next; })} /><span><strong>{note.title}</strong><small>{note.content.slice(0, 100)}</small></span></label>)}{!candidates.length && <p className="muted">没有可加入的记录。</p>}</div>
      {error && adding && <p className="inline-error" role="alert">{error}</p>}
      <footer><span>已选 {selection.size} 条</span><button className="button button-primary" disabled={busy || !selection.size} onClick={() => void run(async () => { if (selectedTopic) await addNotesToSpace(selectedTopic.id, [...selection]); setAdding(false); })}>{busy ? "正在加入…" : "加入空间"}</button></footer>
    </dialog>
  </section>;
}
