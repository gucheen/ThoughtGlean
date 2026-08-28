import { createRef } from "react";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { EditorView } from "@codemirror/view";
import { undo } from "@codemirror/commands";
import { MarkdownEditor, type MarkdownEditorHandle } from "./MarkdownEditor";

const handlers = () => ({ onChange: vi.fn(), onSave: vi.fn(), onCancel: vi.fn(), onPasteImages: vi.fn() });
const getEditor = () => EditorView.findFromDOM(screen.getByRole("textbox", { name: "正文编辑器" }))!;

describe("Markdown editor", () => {
  it("previews inactive Markdown without changing source, then preserves history and selection across modes", () => {
    const value = "正文\n\n## 标题\n\n**重点** 和 `代码`\n\n```text\n原样的代码\n```";
    const callbacks = handlers();
    const app = render(<MarkdownEditor value={value} sourceMode={false} {...callbacks} />);
    const editor = getEditor();
    expect(screen.getByText("标题").closest(".cm-md-h2")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "复制代码" })).toBeInTheDocument();
    expect(editor.state.sliceDoc()).toBe(value);
    expect(callbacks.onChange).not.toHaveBeenCalled();
    act(() => editor.dispatch({ changes: { from: 2, insert: "补充" }, selection: { anchor: 2, head: 4 } }));
    const edited = editor.state.sliceDoc();
    app.rerender(<MarkdownEditor value={edited} sourceMode {...callbacks} />);
    expect(getEditor()).toBe(editor);
    expect(editor.state.selection.main).toMatchObject({ anchor: 2, head: 4 });
    expect(screen.queryByRole("button", { name: "复制代码" })).not.toBeInTheDocument();
    app.rerender(<MarkdownEditor value={edited} sourceMode={false} {...callbacks} />);
    act(() => { undo(editor); });
    expect(editor.state.sliceDoc()).toBe(value);
  });

  it("preserves CRLF and restores source offsets without normalizing the original", () => {
    const value = "标题\r\n\r\n正文";
    const callbacks = handlers();
    const ref = createRef<MarkdownEditorHandle>();
    render(<MarkdownEditor ref={ref} value={value} sourceMode={false} initialSelection={{ anchor: 6, head: 8 }} {...callbacks} />);
    expect(callbacks.onChange).not.toHaveBeenCalled();
    expect(getEditor().state.sliceDoc()).toBe(value);
    expect(ref.current?.getSelection()).toEqual({ anchor: 6, head: 8 });
    act(() => getEditor().dispatch({ changes: { from: getEditor().state.doc.length, insert: "！" } }));
    expect(callbacks.onChange).toHaveBeenLastCalledWith(`${value}！`);
    act(() => ref.current?.insertCode());
    expect(getEditor().state.doc.lines).toBe(6);
    expect(getEditor().state.sliceDoc()).toBe("标题\r\n\r\n```text\r\n正文\r\n```\r\n！");
  });

  it("wraps selected code in a fence that cannot be closed by its contents", () => {
    const value = "前文\n```\n后文";
    const callbacks = handlers();
    const ref = createRef<MarkdownEditorHandle>();
    render(<MarkdownEditor ref={ref} value={value} sourceMode={false} initialSelection={{ anchor: 3, head: 6 }} {...callbacks} />);
    act(() => ref.current?.insertCode());
    expect(getEditor().state.sliceDoc()).toBe("前文\n````text\n```\n````\n后文");
    expect(getEditor().state.sliceDoc(getEditor().state.selection.main.from, getEditor().state.selection.main.to)).toBe("```");
  });

  it("handles image paste and does not submit or cancel during Chinese composition", () => {
    const callbacks = handlers();
    render(<MarkdownEditor value="正文" sourceMode={false} {...callbacks} />);
    const element = screen.getByRole("textbox", { name: "正文编辑器" });
    fireEvent.compositionStart(element);
    fireEvent.keyDown(element, { key: "Enter", ctrlKey: true, isComposing: true });
    fireEvent.keyDown(element, { key: "Escape", isComposing: true });
    expect(callbacks.onSave).not.toHaveBeenCalled();
    expect(callbacks.onCancel).not.toHaveBeenCalled();
    fireEvent.compositionEnd(element);
    const image = new File(["png"], "image.png", { type: "image/png" });
    fireEvent.paste(element, { clipboardData: { files: [image], getData: () => "" } });
    expect(callbacks.onPasteImages).toHaveBeenCalledWith([image]);
  });
});
