// @vitest-environment happy-dom
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const api = vi.hoisted(() => ({
  listIdentityProfiles: vi.fn(),
  getIdentityCapture: vi.fn(),
  getIdentityProfile: vi.fn(),
  createIdentityProfile: vi.fn(),
  confirmIdentityProfile: vi.fn(),
  discardIdentityProfile: vi.fn(),
  armIdentityCapture: vi.fn(),
  disarmIdentityCapture: vi.fn(),
}));
vi.mock("./bridge", () => api);
import { ServiceRequestCompatibility } from "./ServiceRequestCompatibility";
import {
  compatibilityDraft,
  type CompatibilityDraft,
  type IdentityProfileRecord,
} from "./request-compatibility-model";
import { applyLocale } from "./i18n";

const record: IdentityProfileRecord = {
  profile: {
    id: "identity_one",
    service_id: "service_one",
    client: "codex_cli",
    source: "builtin",
    fingerprint: {
      user_agent: "codex_cli_rs/0.160.0",
      version: "0.160.0",
      headers: { Originator: "codex_cli_rs" },
    },
    created_at: "2026-08-01T00:00:00Z",
  },
  etag: `"sha256:${"a".repeat(64)}"`,
};
const idle = { service_id: "service_one", armed: false, rejected: 0 };
const armed = {
  ...idle,
  armed: true,
  client: "codex_cli",
  armed_at: "2026-08-01T00:00:00Z",
  expires_at: "2026-08-01T00:10:00Z",
};
function Harness({
  id = "service_one",
  change = () => {},
}: {
  id?: string;
  change?: (value: CompatibilityDraft) => void;
}) {
  const [value, setValue] = useState(compatibilityDraft());
  return (
    <ServiceRequestCompatibility
      serviceId={id || undefined}
      value={value}
      onChange={(next) => {
        setValue(next);
        change(next);
      }}
    />
  );
}
const button = (name: string, scope: ParentNode = document) =>
  [...scope.querySelectorAll<HTMLButtonElement>("button")].find(
    (node) => node.textContent === name,
  )!;
async function click(name: string, scope: ParentNode = document) {
  await act(async () => button(name, scope).click());
}
async function type(label: string, text: string) {
  const input = document.querySelector<HTMLTextAreaElement>(
    `textarea[aria-label="${label}"]`,
  )!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLTextAreaElement.prototype,
      "value",
    )!.set!.call(input, text);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
describe("ServiceRequestCompatibility", () => {
  let root: Root;
  let container: HTMLDivElement;
  beforeEach(async () => {
    await applyLocale("zh-CN");
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.resetAllMocks();
    api.listIdentityProfiles.mockResolvedValue({
      items: [],
      next_cursor: null,
    });
    api.getIdentityCapture.mockResolvedValue(idle);
    api.createIdentityProfile.mockResolvedValue(record);
    api.getIdentityProfile.mockResolvedValue(record);
    api.armIdentityCapture.mockResolvedValue(armed);
    api.disarmIdentityCapture.mockResolvedValue(idle);
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.useRealTimers();
  });
  it("keeps both JSON buffers and disables profile actions for an unsaved service", async () => {
    const change = vi.fn();
    await act(async () => root.render(<Harness id="" change={change} />));
    expect(api.listIdentityProfiles).not.toHaveBeenCalled();
    expect(button("管理身份档案").disabled).toBe(true);
    await type("模型规则 JSON", '[{"match":"*","body":{}}]');
    await click("默认 headers");
    await type("默认 headers JSON", '{"X-Test":"fake-value"}');
    await click("模型规则");
    expect(document.querySelector<HTMLTextAreaElement>("textarea")!.value).toBe(
      '[{"match":"*","body":{}}]',
    );
    expect(change).toHaveBeenLastCalledWith({
      identity: "",
      rules: '[{"match":"*","body":{}}]',
      headers: '{"X-Test":"fake-value"}',
    });
  });
  it("reviews and confirms a frozen candidate without activating it until explicitly selected", async () => {
    const change = vi.fn();
    const confirmed = {
      ...record,
      etag: `"sha256:${"b".repeat(64)}"`,
      profile: { ...record.profile, confirmed_at: "2026-08-01T00:01:00Z" },
    };
    api.confirmIdentityProfile.mockResolvedValue(confirmed);
    await act(async () => root.render(<Harness change={change} />));
    await click("管理身份档案");
    await click("生成候选");
    expect(api.createIdentityProfile).toHaveBeenCalledWith(
      "service_one",
      "codex_cli",
      "builtin",
    );
    expect(document.querySelector("pre")!.textContent).toContain(
      record.profile.fingerprint.user_agent,
    );
    await click("确认档案");
    expect(api.confirmIdentityProfile).not.toHaveBeenCalled();
    await click("取消", document.querySelector('[role="alertdialog"]')!);
    expect(api.confirmIdentityProfile).not.toHaveBeenCalled();
    await click("确认档案");
    await click("确认档案", document.querySelector('[role="alertdialog"]')!);
    expect(api.confirmIdentityProfile).toHaveBeenCalledWith(
      "service_one",
      record,
    );
    expect(change).not.toHaveBeenCalled();
    await click("用于默认身份（待保存）");
    expect(change).toHaveBeenCalledWith({
      ...compatibilityDraft(),
      identity: "identity_one",
    });
  });
  it("requires capture consent, polls once armed, and opens the captured candidate", async () => {
    vi.useFakeTimers();
    await act(async () => root.render(<Harness />));
    await click("管理身份档案");
    await click("开始采集");
    expect(api.armIdentityCapture).not.toHaveBeenCalled();
    await click("取消", document.querySelector('[role="alertdialog"]')!);
    expect(api.armIdentityCapture).not.toHaveBeenCalled();
    await click("开始采集");
    await click("开始采集", document.querySelector('[role="alertdialog"]')!);
    expect(api.armIdentityCapture).toHaveBeenCalledWith(
      "service_one",
      "codex_cli",
    );
    api.getIdentityCapture.mockResolvedValue({
      ...idle,
      captured_profile: "identity_one",
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(api.getIdentityProfile).toHaveBeenCalledWith(
      "service_one",
      "identity_one",
    );
    expect(document.querySelector("pre")!.textContent).toContain(
      "codex_cli_rs",
    );
    const calls = api.getIdentityCapture.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(api.getIdentityCapture).toHaveBeenCalledTimes(calls);
    expect(api.confirmIdentityProfile).not.toHaveBeenCalled();
  });
  it("pauses capture updates while reviewing a frozen candidate or awaiting confirmation", async () => {
    vi.useFakeTimers();
    api.getIdentityCapture.mockResolvedValue(armed);
    api.confirmIdentityProfile.mockResolvedValue({
      ...record,
      profile: { ...record.profile, confirmed_at: "2026-08-01T00:01:00Z" },
    });
    await act(async () => root.render(<Harness />));
    await click("管理身份档案");
    await click("生成候选");
    await click("确认档案");
    const reads = api.getIdentityCapture.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000);
    });
    expect(api.getIdentityCapture).toHaveBeenCalledTimes(reads);
    await click("确认档案", document.querySelector('[role="alertdialog"]')!);
    expect(api.confirmIdentityProfile).toHaveBeenCalledWith(
      "service_one",
      record,
    );
  });

  it("does not drop a candidate on failed confirmation and prevents double writes", async () => {
    let reject!: (error: Error) => void;
    api.confirmIdentityProfile.mockImplementation(
      () =>
        new Promise((_resolve, fail) => {
          reject = fail;
        }),
    );
    await act(async () => root.render(<Harness />));
    await click("管理身份档案");
    await click("生成候选");
    await click("确认档案");
    const action = button(
      "确认档案",
      document.querySelector('[role="alertdialog"]')!,
    );
    await act(async () => {
      action.click();
      action.click();
    });
    expect(api.confirmIdentityProfile).toHaveBeenCalledTimes(1);
    await act(async () => reject(new Error("ETag conflict")));
    expect(document.querySelector('[role="alert"]')!.textContent).toContain(
      "ETag conflict",
    );
    expect(document.querySelector("pre")!.textContent).toContain(
      "codex_cli_rs",
    );
    expect(button("丢弃候选")).toBeTruthy();
  });
  it("stops an armed window only on explicit action and loads all profile pages", async () => {
    api.listIdentityProfiles
      .mockResolvedValueOnce({ items: [record.profile], next_cursor: "next" })
      .mockResolvedValueOnce({ items: [], next_cursor: null });
    api.getIdentityCapture.mockResolvedValue(armed);
    await act(async () => root.render(<Harness />));
    expect(api.listIdentityProfiles).toHaveBeenCalledWith(
      "service_one",
      "next",
    );
    await click("管理身份档案");
    await click("停止采集");
    expect(api.disarmIdentityCapture).toHaveBeenCalledWith("service_one");
  });
});
