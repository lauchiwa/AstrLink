// @vitest-environment happy-dom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FadeLine } from "./FadeLine";

let container: HTMLDivElement;
let root: Root;

/** Lays the line out as if its text took `scroll` pixels in a `client` box. */
function layout(scroll: number, client: number) {
  vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockReturnValue(scroll);
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(client);
}

function line() {
  act(() => root.render(<FadeLine text="一段很长的回复" />));
  return container.querySelector<HTMLElement>('[data-slot="fade-line"]')!;
}

describe("FadeLine", () => {
  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.restoreAllMocks();
  });

  it("fades the end of text that runs past the width", () => {
    layout(600, 300);
    const element = line();
    expect(element.textContent).toBe("一段很长的回复");
    expect(element.dataset.clipped).toBe("true");
    expect(element.style.maskImage).toContain("linear-gradient");
  });

  it("leaves text that fits unfaded", () => {
    layout(300, 300);
    const element = line();
    expect(element.dataset.clipped).toBeUndefined();
    expect(element.style.maskImage || "").toBe("");
  });
});
