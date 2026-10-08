// @vitest-environment happy-dom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "./dialog";

describe("workspace dialogs", () => {
  let container: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    document.body.innerHTML = "";
  });

  it.each(["default", "workspace"] as const)(
    "keeps the %s variant's sizing and scrolling contract",
    async (variant) => {
      await act(async () => {
        root.render(
          <Dialog open>
            <DialogContent variant={variant} className="sm:max-w-2xl">
              <DialogTitle>Editor</DialogTitle>
              <DialogDescription>Dialog sizing fixture</DialogDescription>
            </DialogContent>
          </Dialog>,
        );
      });
      const content = document.querySelector('[data-slot="dialog-content"]')!;
      expect(content.classList.contains("sm:max-w-2xl")).toBe(true);
      if (variant === "workspace") {
        // Center within usable height, not underneath the native title bar.
        expect(
          content.classList.contains(
            "top-[calc(50%+var(--window-chrome-height)/2)]",
          ),
        ).toBe(true);
        expect(
          content.classList.contains(
            "h-[calc(100dvh-var(--window-chrome-height)-3rem)]",
          ),
        ).toBe(true);
        expect(content.classList.contains("overflow-hidden")).toBe(true);
        expect(content.classList.contains("overflow-y-auto")).toBe(false);
      } else {
        expect(content.classList.contains("top-[50%]")).toBe(true);
        expect(content.classList.contains("overflow-y-auto")).toBe(true);
      }
    },
  );
});
