import { memo, useState, type ClipboardEvent } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";

type SourceNode = {
  type: string;
  tagName?: string;
  value?: string;
  properties?: Record<string, unknown>;
  position?: { start: { offset?: number }; end: { offset?: number } };
  children?: SourceNode[];
};

// Text spans retain source offsets so a reading selection can survive entering the editor.
function sourcePositions() {
  return (tree: SourceNode) => {
    const visit = (node: SourceNode) => {
      if (!node.children) return;
      node.children = node.children.map(child => {
        if (child.type === "text" && child.position) {
          return { type: "element", tagName: "span", properties: {
            "data-source-start": child.position.start.offset,
            "data-source-end": child.position.end.offset,
          }, children: [child] };
        }
        if (child.type === "element" && child.tagName === "code" && node.tagName !== "pre" && child.position) {
          child.properties = { ...child.properties, "data-source-start": child.position.start.offset, "data-source-end": child.position.end.offset };
        }
        visit(child);
        return child;
      });
    };
    visit(tree);
  };
}

export function CopyButton({ text, label = "复制 Markdown" }: { text: string; label?: string }) {
  const [status, setStatus] = useState("");
  return <button type="button" className="text-button" onClick={async () => {
    try {
      await navigator.clipboard.writeText(text);
      setStatus("已复制");
    } catch { setStatus("复制失败，请重试"); }
  }} aria-label={label} title={status || label}>{status || label}</button>;
}

function textOf(node: SourceNode): string {
  return node.value ?? node.children?.map(textOf).join("") ?? "";
}

export const MarkdownContent = memo(function MarkdownContent({ content }: { content: string }) {
  return <Markdown remarkPlugins={[remarkGfm]} rehypePlugins={[sourcePositions]} components={{
    a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
    img: ({ node: _node, ...props }) => <img {...props} loading="lazy" referrerPolicy="no-referrer" />,
    pre: ({ node, children }) => {
      const code = node?.children.find(child => child.type === "element" && child.tagName === "code");
      const value = code ? textOf(code).replace(/\n$/, "") : "";
      const language = code?.type === "element" ? String(code.properties.className ?? "").replace(/^language-/, "") : "";
      const start = node?.position?.start.offset ?? 0;
      const opening = content.slice(start).match(/^ {0,3}(?:`{3,}|~{3,})[^\r\n]*\r?\n/);
      const offset = content.indexOf(value, start + (opening?.[0].length ?? 0));
      return <section className="code-block">
        <header data-selection-ignore="true"><span>{language || "text"}</span><CopyButton text={value} label="复制代码" /></header>
        <pre data-source-start={offset < 0 ? start : offset} data-source-end={(offset < 0 ? start : offset) + value.length}>{children}</pre>
      </section>;
    },
  }}>{content}</Markdown>;
});

export type SourceSelection = { anchor: number; head: number };
export type SourceViewport = { position: number; top: number };

export function readingViewport(root: HTMLElement | null): SourceViewport | undefined {
  const elements = root?.querySelectorAll<HTMLElement>("[data-source-start]");
  if (!elements) return;
  for (const element of elements) {
    const rect = element.getBoundingClientRect();
    if (rect.height && rect.bottom > 280) return { position: Number(element.dataset.sourceStart), top: rect.top };
  }
}

export function restoreReadingViewport(root: HTMLElement | null, viewport: SourceViewport | undefined) {
  if (!root || !viewport) return;
  const elements = Array.from(root.querySelectorAll<HTMLElement>("[data-source-start]"));
  const element = elements.find(element => Number(element.dataset.sourceStart) <= viewport.position && Number(element.dataset.sourceEnd) >= viewport.position)
    ?? elements.find(element => Number(element.dataset.sourceStart) >= viewport.position);
  if (element) window.scrollBy({ top: element.getBoundingClientRect().top - viewport.top });
}

function sourceOffset(root: HTMLElement, node: Node | null, offset: number, source: string): number | undefined {
  if (!node || !root.contains(node)) return;
  const element = (node instanceof Element ? node : node.parentElement)?.closest<HTMLElement>("[data-source-start]");
  if (!element || !root.contains(element)) return;
  let start = Number(element.dataset.sourceStart);
  const end = Number(element.dataset.sourceEnd);
  const range = document.createRange();
  range.setStart(element, 0);
  range.setEnd(node, offset);
  const visible = element.textContent ?? "";
  const count = range.toString().length;
  let raw = source.slice(start, end);
  if (element.closest("code") && raw.startsWith("`")) {
    const fence = raw.match(/^`+/)![0].length;
    start += fence; raw = raw.slice(fence, -fence);
    if (raw.startsWith(" ") && raw.endsWith(" ") && raw.trim()) { start++; raw = raw.slice(1, -1); }
  }
  if (raw === visible) return start + count;
  let position = 0;
  let displayed = 0;
  const code = Boolean(element.closest("code, pre"));
  while (displayed < count && position < raw.length) {
    const rest = raw.slice(position);
    const entity = !code && /^&(?:#\d+|#x[\da-f]+|[a-z][\da-z]+);/i.exec(rest)?.[0];
    if (entity) {
      const decoder = document.createElement("textarea");
      decoder.innerHTML = entity;
      displayed += decoder.value.length; position += entity.length;
    } else if (!code && /^\\[!"#$%&'()*+,\-./:;<=>?@[\]\\^_`{|}~]/.test(rest)) {
      displayed++; position += 2;
    } else if (rest.startsWith("\r\n")) {
      displayed++; position += 2;
    } else { displayed++; position++; }
  }
  return Math.min(end, start + position);
}

export function readingSelection(root: HTMLElement | null, content: string): SourceSelection | undefined {
  const selection = window.getSelection();
  if (!root || !selection?.rangeCount || selection.isCollapsed) return;
  const anchor = sourceOffset(root, selection.anchorNode, selection.anchorOffset, content);
  const head = sourceOffset(root, selection.focusNode, selection.focusOffset, content);
  if (anchor === undefined || head === undefined) return;
  return { anchor, head };
}

export function copyReadingSelection(event: ClipboardEvent<HTMLElement>) {
  const selection = window.getSelection();
  if (!selection?.rangeCount || selection.isCollapsed) return;
  const fragment = selection.getRangeAt(0).cloneContents();
  if (!fragment.querySelector("[data-selection-ignore]")) return;
  fragment.querySelectorAll("[data-selection-ignore]").forEach(element => element.remove());
  const container = document.createElement("div");
  container.append(fragment);
  container.querySelectorAll("p, pre, h1, h2, h3, h4, h5, h6, li, blockquote").forEach(element => element.append("\n"));
  event.clipboardData.setData("text/plain", container.textContent?.trimEnd() ?? "");
  event.clipboardData.setData("text/html", container.innerHTML);
  event.preventDefault();
}
