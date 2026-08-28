import { useImperativeHandle, useLayoutEffect, useRef, type Ref } from "react";
import { Compartment, EditorState, StateField, Text, Transaction, type Range } from "@codemirror/state";
import { Decoration, EditorView, WidgetType, keymap, type DecorationSet } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import { defaultHighlightStyle, syntaxHighlighting, syntaxTree } from "@codemirror/language";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import type { SourceSelection, SourceViewport } from "./MarkdownContent";

class LabelWidget extends WidgetType {
  constructor(readonly label: string, readonly className: string) { super(); }
  eq(other: LabelWidget) { return other.label === this.label && other.className === this.className; }
  toDOM() {
    const span = document.createElement("span");
    span.className = this.className;
    span.textContent = this.label;
    return span;
  }
}

class CodeHeaderWidget extends WidgetType {
  constructor(readonly language: string, readonly code: string) { super(); }
  eq(other: CodeHeaderWidget) { return other.language === this.language && other.code === this.code; }
  toDOM() {
    const header = document.createElement("span");
    header.className = "cm-md-code-header";
    const label = document.createElement("span");
    label.textContent = this.language || "text";
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = "复制代码";
    button.setAttribute("aria-label", "复制代码");
    button.addEventListener("mousedown", event => event.preventDefault());
    button.addEventListener("click", async () => {
      try { await navigator.clipboard.writeText(this.code); button.textContent = "已复制"; }
      catch { button.textContent = "复制失败"; }
    });
    header.append(label, button);
    return header;
  }
}

// Decorations only change presentation. The document remains the original Markdown, including fences.
export function markdownDecorations(state: EditorState): DecorationSet {
  const ranges: Range<Decoration>[] = [];
  const codeLines = new Set<number>();
  const selection = state.selection.main;
  const activeLine = selection.empty ? state.doc.lineAt(selection.head).number : -1;
  const active = (position: number) => state.doc.lineAt(position).number === activeLine;
  const mark = (from: number, to: number, className: string) => {
    if (to > from) ranges.push(Decoration.mark({ class: className }).range(from, to));
  };
  const line = (from: number, className: string) => ranges.push(Decoration.line({ class: className }).range(state.doc.lineAt(from).from));
  const hide = (from: number, to: number, widget?: WidgetType) => {
    if (to > from) ranges.push(Decoration.replace({ widget }).range(from, to));
  };
  syntaxTree(state).iterate({ enter(ref) {
    const { name, from, to, node } = ref;
    const heading = /^(?:ATX|Setext)Heading(\d)$/.exec(name);
    if (heading) line(from, `cm-md-heading cm-md-h${heading[1]}`);
    if (name === "StrongEmphasis") mark(from, to, "cm-md-strong");
    if (name === "Emphasis") mark(from, to, "cm-md-emphasis");
    if (name === "Strikethrough") mark(from, to, "cm-md-strike");
    if (name === "InlineCode") mark(from, to, "cm-md-inline-code");
    if (name === "Link" || name === "Autolink") mark(from, to, "cm-md-link");
    if (name === "Blockquote") {
      for (let number = state.doc.lineAt(from).number; number <= state.doc.lineAt(to).number; number++) line(state.doc.line(number).from, "cm-md-quote");
    }
    if (name === "ListItem") line(from, "cm-md-list");
    if (name === "Table") {
      for (let number = state.doc.lineAt(from).number; number <= state.doc.lineAt(to).number; number++) line(state.doc.line(number).from, "cm-md-table");
    }
    if (name === "FencedCode" || name === "CodeBlock") {
      const first = state.doc.lineAt(from);
      const last = state.doc.lineAt(to);
      for (let number = first.number; number <= last.number; number++) { line(state.doc.line(number).from, "cm-md-code"); codeLines.add(number); }
      if (name === "FencedCode") {
        const fences = node.getChildren("CodeMark");
        const closing = fences.length > 1 ? fences[fences.length - 1] : undefined;
        const codeEnd = closing ? Math.max(first.to, state.doc.lineAt(closing.from).from - 1) : to;
        const code = state.doc.sliceString(Math.min(first.to + 1, to), codeEnd);
        if (first.number < last.number) line(first.to + 1, "cm-md-code-first");
        if (closing && last.number > first.number + 1) line(state.doc.line(last.number - 1).from, "cm-md-code-last");
        if (!active(first.from)) {
          line(first.from, "cm-md-fence-start");
          hide(first.from, first.to, new CodeHeaderWidget(node.getChild("CodeInfo") ? state.doc.sliceString(node.getChild("CodeInfo")!.from, node.getChild("CodeInfo")!.to) : "", code));
        }
        if (closing && !active(closing.from)) {
          line(closing.from, "cm-md-fence-end");
          hide(closing.from, closing.to);
        }
      }
      return false;
    }
    if (name === "HorizontalRule" && !active(from)) {
      line(from, "cm-md-rule");
      hide(from, to, new LabelWidget("", "cm-md-rule-line"));
    }
    if (name === "ListMark" && !active(from) && /^[-+*]$/.test(state.doc.sliceString(from, to))) hide(from, to, new LabelWidget("•", "cm-md-bullet"));
    if (name === "TaskMarker" && !active(from)) hide(from, to, new LabelWidget(/x/i.test(state.doc.sliceString(from, to)) ? "☑" : "☐", "cm-md-task"));
    if (["HeaderMark", "QuoteMark", "EmphasisMark", "StrikethroughMark", "CodeMark", "LinkMark"].includes(name)) {
      if (active(from)) mark(from, to, "cm-md-syntax");
      else {
        let end = to;
        if ((name === "HeaderMark" || name === "QuoteMark") && state.doc.sliceString(to, to + 1) === " ") end++;
        hide(from, end);
      }
    }
    if ((name === "URL" || name === "LinkTitle") && node.parent?.name === "Link" && !active(from)) hide(from, to);
  } });
  for (let number = 1; number <= state.doc.lines; number++) {
    const current = state.doc.line(number);
    if (!current.length && !codeLines.has(number)) line(current.from, "cm-md-blank");
  }
  return Decoration.set(ranges, true);
}

const livePreview = StateField.define<DecorationSet>({
  create: markdownDecorations,
  update: (decorations, transaction) => transaction.docChanged || transaction.selection || syntaxTree(transaction.startState) !== syntaxTree(transaction.state) ? markdownDecorations(transaction.state) : decorations,
  provide: field => EditorView.decorations.from(field),
});

export type MarkdownEditorHandle = {
  focus: () => void;
  insertCode: () => void;
  getSelection: () => SourceSelection;
  getViewport: () => SourceViewport | undefined;
};

function documentOffset(source: string, offset: number) { return source.slice(0, offset).replace(/\r\n/g, "\n").length; }
function sourceOffset(state: EditorState, offset: number) { return state.sliceDoc(0, offset).length; }

type Props = {
  ref?: Ref<MarkdownEditorHandle>;
  value: string;
  onChange: (value: string) => void;
  sourceMode: boolean;
  initialSelection?: SourceSelection;
  initialViewport?: SourceViewport;
  disabled?: boolean;
  onSave: () => void;
  onCancel: () => void;
  onPasteImages: (files: File[]) => void;
};

export function MarkdownEditor({ ref, ...props }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView>(null);
  const callbacks = useRef(props);
  callbacks.current = props;
  const preview = useRef(new Compartment());
  const editable = useRef(new Compartment());
  useImperativeHandle(ref, () => ({
    focus: () => view.current?.contentDOM.focus({ preventScroll: true }),
    getSelection: () => {
      const selection = view.current?.state.selection.main;
      const state = view.current?.state;
      return state && selection ? { anchor: sourceOffset(state, selection.anchor), head: sourceOffset(state, selection.head) } : { anchor: 0, head: 0 };
    },
    getViewport: () => {
      const editor = view.current;
      if (!editor) return;
      const bounds = editor.contentDOM.getBoundingClientRect();
      const position = editor.posAtCoords({ x: bounds.left + 8, y: Math.max(280, bounds.top) });
      const coordinates = position === null ? null : editor.coordsAtPos(position);
      return position !== null && coordinates ? { position: sourceOffset(editor.state, position), top: coordinates.top } : undefined;
    },
    insertCode: () => {
      const editor = view.current;
      if (!editor || editor.state.readOnly) return;
      const { from, to } = editor.state.selection.main;
      const selected = editor.state.doc.sliceString(from, to);
      const before = from > 0 && editor.state.doc.sliceString(from - 1, from) !== "\n" ? "\n" : "";
      const after = to < editor.state.doc.length && editor.state.doc.sliceString(to, to + 1) !== "\n" ? "\n" : "";
      const fence = "`".repeat(Math.max(3, ...[...selected.matchAll(/`+/g)].map(match => match[0].length + 1)));
      const opening = `${before}${fence}text\n`;
      editor.dispatch({ changes: { from, to, insert: Text.of(`${opening}${selected}\n${fence}${after}`.split("\n")) }, selection: { anchor: from + opening.length, head: from + opening.length + selected.length }, scrollIntoView: true, userEvent: "input" });
      editor.focus();
    },
  }), []);

  useLayoutEffect(() => {
    const initial = callbacks.current;
    const editor = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: initial.value,
        selection: initial.initialSelection ? { anchor: documentOffset(initial.value, initial.initialSelection.anchor), head: documentOffset(initial.value, initial.initialSelection.head) } : undefined,
        extensions: [
          EditorState.lineSeparator.of(initial.value.includes("\r\n") ? "\r\n" : "\n"),
          history(),
          markdown({ base: markdownLanguage, completeHTMLTags: false, pasteURLAsLink: false }),
          EditorView.lineWrapping,
          EditorView.contentAttributes.of({ "aria-label": "正文编辑器", "aria-multiline": "true", spellcheck: "false" }),
          keymap.of([
            { key: "Mod-Enter", run: editor => { if (editor.compositionStarted) return false; callbacks.current.onSave(); return true; } },
            { key: "Escape", run: editor => { if (editor.compositionStarted) return false; callbacks.current.onCancel(); return true; } },
            ...defaultKeymap, ...historyKeymap,
          ]),
          preview.current.of(callbacks.current.sourceMode ? syntaxHighlighting(defaultHighlightStyle) : livePreview),
          editable.current.of([EditorState.readOnly.of(Boolean(callbacks.current.disabled)), EditorView.editable.of(!callbacks.current.disabled)]),
          EditorView.updateListener.of(update => {
            if (update.docChanged) callbacks.current.onChange(update.state.sliceDoc());
          }),
          EditorView.domEventHandlers({ paste(event) {
            const images = Array.from(event.clipboardData?.files ?? []).filter(file => file.type.startsWith("image/"));
            if (!images.length) return false;
            event.preventDefault(); callbacks.current.onPasteImages(images); return true;
          } }),
        ],
      }),
    });
    view.current = editor;
    editor.contentDOM.focus({ preventScroll: true });
    const viewport = initial.initialViewport;
    let restoreFrame = 0;
    if (viewport) {
      const position = Math.min(documentOffset(initial.value, viewport.position), editor.state.doc.length);
      editor.requestMeasure({
        read: view => view.coordsAtPos(position),
        // Restore after CodeMirror finishes its own scroll anchoring for this measurement.
        write: () => { restoreFrame = requestAnimationFrame(() => {
          const coordinates = editor.coordsAtPos(position);
          if (coordinates) window.scrollBy({ top: coordinates.top - viewport.top });
        }); },
      });
    }
    return () => { cancelAnimationFrame(restoreFrame); editor.destroy(); view.current = null; };
  }, []);

  useLayoutEffect(() => {
    const editor = view.current;
    if (!editor) return;
    if (editor.state.sliceDoc() !== props.value) {
      editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: props.value }, annotations: Transaction.addToHistory.of(false) });
    }
  }, [props.value]);

  useLayoutEffect(() => {
    const editor = view.current;
    if (!editor) return;
    const scrollY = window.scrollY;
    editor.dispatch({ effects: preview.current.reconfigure(props.sourceMode ? syntaxHighlighting(defaultHighlightStyle) : livePreview) });
    window.scrollTo({ top: scrollY });
  }, [props.sourceMode]);

  useLayoutEffect(() => {
    view.current?.dispatch({ effects: editable.current.reconfigure([EditorState.readOnly.of(Boolean(props.disabled)), EditorView.editable.of(!props.disabled)]) });
  }, [props.disabled]);

  return <div ref={host} className={`markdown-editor ${props.sourceMode ? "is-source" : "is-live-preview"}`} />;
}
