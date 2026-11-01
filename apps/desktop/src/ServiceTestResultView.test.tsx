// @vitest-environment happy-dom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it } from "vitest";

import { ServiceTestResultView } from "./ServiceTestResultView";
import type { ServiceTestResult } from "./service-test-model";

let root: Root;
let container: HTMLDivElement;

beforeEach(() => {
  (
    globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
  ).IS_REACT_ACT_ENVIRONMENT = true;
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

const completed: ServiceTestResult = {
  service_id: "service_test",
  protocol: "openai.chat",
  model: "test-model",
  stream: true,
  ok: true,
  status_code: 200,
  duration_ms: 100,
  output: "OK",
};

function statusTone() {
  return container
    .querySelector('[role="status"] [data-tone]')
    ?.getAttribute("data-tone");
}

// Upstream's cancellation stays neutral, and the fork keeps a completed
// request neutral too: completion does not prove the model is usable.
it.each([
  // An error is present too, so only the cancelled branch keeps it neutral.
  [
    "cancelled",
    { cancelled: true, error: "service_test_cancelled" },
    "测试已取消",
    "neutral",
  ],
  ["completed", { result: completed }, "请求完成", "neutral"],
  ["failed", { result: { ...completed, ok: false } }, "测试失败", "negative"],
] as const)(
  "gives a %s test its status tone",
  async (_name, props, text, tone) => {
    await act(async () => root.render(<ServiceTestResultView {...props} />));
    expect(container.querySelector('[role="status"]')?.textContent).toContain(
      text,
    );
    expect(statusTone()).toBe(tone);
  },
);
