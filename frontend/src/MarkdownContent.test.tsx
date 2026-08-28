import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MarkdownContent, copyReadingSelection, readingSelection } from "./MarkdownContent";

afterEach(() => window.getSelection()?.removeAllRanges());

describe("Markdown reading", () => {
  it("renders Markdown consistently and never enables raw HTML or unsafe links", () => {
    const { container } = render(<MarkdownContent content={'## 标题\n\n- 列表\n\n> 引用\n\n**重点** 和 `代码`\n\n| A | B |\n| - | - |\n| a | b |\n\n<script>alert(1)</script>\n\n[危险](javascript:alert%281%29)'} />);
    expect(screen.getByRole("heading", { name: "标题", level: 2 })).toBeInTheDocument();
    expect(screen.getByRole("listitem")).toHaveTextContent("列表");
    expect(container.querySelector("blockquote")).toHaveTextContent("引用");
    expect(container.querySelector("strong")).toHaveTextContent("重点");
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
  });

  it("maps a reading selection across formatted text and code back to the exact source", () => {
    const content = "正文 **需要保留** 的内容\n\n```text\n第一行\n第二行\n```\n\n后文";
    const { container } = render(<MarkdownContent content={content} />);
    const start = screen.getByText("需要保留").firstChild!;
    const end = screen.getByText("后文").firstChild!;
    const selection = window.getSelection()!;
    selection.setBaseAndExtent(start, 1, end, 1);
    expect(readingSelection(container, content)).toEqual({ anchor: content.indexOf("需要保留") + 1, head: content.indexOf("后文") + 1 });
    const code = container.querySelector("pre code")!.firstChild!;
    selection.setBaseAndExtent(code, 1, code, 3);
    expect(readingSelection(container, content)).toEqual({ anchor: content.indexOf("第一行") + 1, head: content.indexOf("第一行") + 3 });
  });

  it("maps decoded entities, escapes and inline code without counting hidden syntax", () => {
    const content = "A &amp; B \\* C 和 `内联代码`";
    const { container } = render(<MarkdownContent content={content} />);
    const text = screen.getByText("A & B * C 和").firstChild!;
    window.getSelection()!.setBaseAndExtent(text, 3, text, 7);
    expect(readingSelection(container, content)).toEqual({ anchor: 7, head: 12 });
    const code = screen.getByText("内联代码").firstChild!;
    window.getSelection()!.setBaseAndExtent(code, 1, code, 3);
    expect(readingSelection(container, content)).toEqual({ anchor: content.indexOf("内联代码") + 1, head: content.indexOf("内联代码") + 3 });
  });

  it("does not mistake a fence language for matching code content", () => {
    const content = "```text\ntext\n```";
    const { container } = render(<MarkdownContent content={content} />);
    const code = container.querySelector("pre code")!.firstChild!;
    window.getSelection()!.setBaseAndExtent(code, 0, code, 4);
    expect(readingSelection(container, content)).toEqual({ anchor: 8, head: 12 });
  });

  it("copies across code blocks without including code toolbar labels", () => {
    const { container } = render(<div onCopy={copyReadingSelection}><MarkdownContent content={"前文\n\n```js\nconst a = 1;\n```\n\n后文"} /></div>);
    const selection = window.getSelection()!;
    selection.setBaseAndExtent(screen.getByText("前文").firstChild!, 0, screen.getByText("后文").firstChild!, 2);
    const setData = vi.fn();
    fireEvent.copy(container.firstChild!, { clipboardData: { setData } });
    const plain = setData.mock.calls.find(([type]) => type === "text/plain")?.[1];
    expect(plain).toContain("前文");
    expect(plain).toContain("const a = 1;");
    expect(plain).toContain("后文");
    expect(plain).not.toContain("复制代码");
    expect(plain).not.toContain("js");
  });
});
