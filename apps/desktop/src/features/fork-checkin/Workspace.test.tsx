// @vitest-environment happy-dom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const bridgeMocks = vi.hoisted(() => ({
  loadCheckinAvailability: vi.fn(),
  listCheckinAccounts: vi.fn(),
  listCheckinJobs: vi.fn(),
  updateCheckinSettings: vi.fn(),
  createCheckinAccount: vi.fn(),
  updateCheckinAccount: vi.fn(),
  deleteCheckinAccount: vi.fn(),
}));

vi.mock("./bridge", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./bridge")>()),
  ...bridgeMocks,
}));

import { CheckinBridgeError } from "./bridge";
import type { CheckinAccount, CheckinJob, CheckinStatus } from "./model";
import { CheckinWorkspace } from "./Workspace";

const readyStatus: CheckinStatus = {
  protocol_version: 1,
  present: true,
  enabled: true,
  storage_ready: true,
  scheduler_running: true,
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

const disabledStatus: CheckinStatus = {
  ...readyStatus,
  enabled: false,
  storage_ready: false,
  scheduler_running: false,
};

function account(index: number): CheckinAccount {
  return {
    id: `acct_${String(index).padStart(3, "0")}`,
    dashboard_base_url: `https://relay${index}.example/console`,
    state: index % 2 ? "connected" : "draft",
    revision: 1,
    network: { mode: "direct" },
    time_zone: "Asia/Shanghai",
    automatic: false,
    ...(index % 2 ? { remote_user_id: String(index) } : {}),
    bound_services: [],
    config_fingerprint: "a".repeat(64),
  };
}

const finishedJob: CheckinJob = {
  id: "job_one",
  account_id: "acct_001",
  action: "check_in",
  status: "success",
  dispatched: true,
  proof_source: "submission_response",
  reward: { known: false },
  created_at: "2026-01-05T08:00:00Z",
  finished_at: "2026-01-05T08:00:04Z",
  children: [],
};

const uncertainJob: CheckinJob = {
  ...finishedJob,
  id: "job_two",
  status: "uncertain",
  proof_source: "none",
  reward: undefined,
  failure_code: "unconfirmed",
};

describe("CheckinWorkspace", () => {
  let container: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.resetAllMocks();
    bridgeMocks.listCheckinAccounts.mockResolvedValue({ items: [] });
    bridgeMocks.listCheckinJobs.mockResolvedValue({ items: [] });
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    document.body.innerHTML = "";
  });

  const render = async (isReady = true, session = "session-1") => {
    await act(async () => {
      root.render(
        <CheckinWorkspace coreSessionKey={session} isReady={isReady} />,
      );
    });
    await act(async () => {
      await Promise.resolve();
    });
  };

  const button = (label: string) => {
    const match = [...document.querySelectorAll("button")].find(
      (node) => node.textContent?.trim() === label,
    );
    if (!match) throw new Error(`missing button ${label}`);
    return match;
  };

  const fill = async (label: string, value: string) => {
    await act(async () => {
      const input = document.querySelector<HTMLInputElement>(
        `input[aria-label="${label}"]`,
      )!;
      Object.getOwnPropertyDescriptor(
        HTMLInputElement.prototype,
        "value",
      )!.set!.call(input, value);
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
  };
  const confirmTarget = async () => {
    await act(async () =>
      [...document.querySelectorAll<HTMLButtonElement>('[role="checkbox"]')]
        .find((item) =>
          item.closest("label")?.textContent?.includes("我已核对"),
        )!
        .click(),
    );
  };
  const rowAction = async (label: string) => {
    await act(async () =>
      document
        .querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`)!
        .click(),
    );
  };

  it("shows a new draft immediately and deduplicates later cursor pages", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts
      .mockResolvedValueOnce({ items: [account(1)], next_cursor: "next-page" })
      .mockResolvedValueOnce({ items: [account(2), account(3)] });
    bridgeMocks.createCheckinAccount.mockResolvedValue(account(2));
    await render();
    await act(async () => button("添加签到账号").click());
    await fill("后台地址", account(2).dashboard_base_url);
    await confirmTarget();
    await act(async () => button("添加账号").click());
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(
      container.querySelectorAll('[data-testid="checkin-account-row"]'),
    ).toHaveLength(2);
    expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(1);
    await act(async () => button("加载更多").click());
    expect(
      container.querySelectorAll('[data-testid="checkin-account-row"]'),
    ).toHaveLength(3);
  });

  it("does not let an old page response replace a saved account", async () => {
    const page = deferred<{ items: CheckinAccount[] }>();
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts
      .mockResolvedValueOnce({ items: [account(1)], next_cursor: "next-page" })
      .mockReturnValueOnce(page.promise);
    bridgeMocks.updateCheckinAccount.mockResolvedValue({
      ...account(1),
      revision: 2,
      time_zone: "UTC",
    });
    await render();
    await act(async () => button("加载更多").click());
    await rowAction("编辑 relay1.example");
    await fill("时区", "UTC");
    await confirmTarget();
    await act(async () => button("保存").click());
    await act(async () => page.resolve({ items: [account(1), account(2)] }));
    expect(
      container.querySelectorAll('[data-testid="checkin-account-row"]'),
    ).toHaveLength(1);
    expect(container.textContent).toContain("UTC");
    expect(container.textContent).not.toContain("Asia/Shanghai");
  });

  it("retires an account editor on a Core replacement and ignores the old write", async () => {
    const pending = deferred<CheckinAccount>();
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts
      .mockResolvedValueOnce({ items: [account(1)] })
      .mockResolvedValueOnce({ items: [account(2)] });
    bridgeMocks.updateCheckinAccount.mockReturnValue(pending.promise);
    await render();
    await rowAction("编辑 relay1.example");
    await fill("时区", "UTC");
    await confirmTarget();
    await act(async () => button("保存").click());
    await render(true, "session-2");
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    await act(async () => pending.resolve({ ...account(1), time_zone: "UTC" }));
    expect(container.textContent).not.toContain("relay1.example");
    expect(container.textContent).toContain("relay2.example");
    expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(2);
  });

  it("removes a deleted account and its visible records without another write", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts.mockResolvedValue({ items: [account(1)] });
    bridgeMocks.listCheckinJobs.mockResolvedValue({ items: [finishedJob] });
    bridgeMocks.deleteCheckinAccount.mockResolvedValue(undefined);
    await render();
    await rowAction("删除 relay1.example");
    await act(async () => button("删除").click());
    expect(
      container.querySelectorAll('[data-testid="checkin-account-row"]'),
    ).toHaveLength(0);
    expect(
      container.querySelectorAll('[data-testid="checkin-job-row"]'),
    ).toHaveLength(0);
    expect(bridgeMocks.deleteCheckinAccount).toHaveBeenCalledOnce();
  });

  it.each([
    ["edit", "success"],
    ["edit", "failure"],
    ["delete", "success"],
    ["delete", "failure"],
  ] as const)(
    "retires a pending %s on disconnect and drops its late %s",
    async (action, outcome) => {
      const pending = deferred<CheckinAccount | void>();
      bridgeMocks.loadCheckinAvailability.mockResolvedValue({
        kind: "present",
        status: readyStatus,
      });
      bridgeMocks.listCheckinAccounts
        .mockResolvedValueOnce({ items: [account(1)] })
        .mockResolvedValueOnce({ items: [account(2)] });
      const write =
        action === "edit"
          ? bridgeMocks.updateCheckinAccount
          : bridgeMocks.deleteCheckinAccount;
      write.mockReturnValue(pending.promise);
      await render();
      if (action === "edit") {
        await rowAction("编辑 relay1.example");
        await fill("时区", "UTC");
        await confirmTarget();
        await act(async () => button("保存").click());
      } else {
        await rowAction("删除 relay1.example");
        await act(async () => button("删除").click());
      }
      await render(false);
      expect(
        document.querySelector('[role="dialog"], [role="alertdialog"]'),
      ).toBeNull();
      await act(async () => {
        if (outcome === "success") pending.resolve(account(1));
        else pending.reject(new CheckinBridgeError("transport"));
      });
      expect(container.textContent).toContain("等待本地网关");
      expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(1);
      expect(write).toHaveBeenCalledTimes(1);
      await render(true, "session-2");
      expect(container.textContent).toContain("relay2.example");
      expect(container.textContent).not.toContain("relay1.example");
      expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(2);
      expect(write).toHaveBeenCalledTimes(1);
    },
  );

  it("makes no extension call before the gateway is ready", async () => {
    await render(false);

    expect(container.textContent).toContain("等待本地网关");
    expect(bridgeMocks.loadCheckinAvailability).not.toHaveBeenCalled();
    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
  });

  it("shows a Core without the extension as unavailable, not as a failure", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "unavailable",
    });

    await render();

    expect(container.textContent).toContain("当前网关不包含签到功能");
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
  });

  it("shows a protocol mismatch as unsupported", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "unsupported",
    });

    await render();

    expect(container.textContent).toContain("网关的签到协议版本不一致");
    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
  });

  it("offers to turn check-in on without reading accounts while it is off", async () => {
    bridgeMocks.loadCheckinAvailability
      .mockResolvedValueOnce({
        kind: "present",
        status: {
          ...readyStatus,
          enabled: false,
          storage_ready: false,
          scheduler_running: false,
        },
      })
      .mockResolvedValueOnce({ kind: "present", status: readyStatus });
    bridgeMocks.updateCheckinSettings.mockResolvedValue({ enabled: true });

    await render();

    expect(container.textContent).toContain("中转站签到未开启");
    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
    expect(bridgeMocks.listCheckinJobs).not.toHaveBeenCalled();

    await act(async () => button("开启签到").click());
    await act(async () => {
      await Promise.resolve();
    });

    expect(bridgeMocks.updateCheckinSettings).toHaveBeenCalledWith(true, null);
    expect(bridgeMocks.listCheckinAccounts).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain("还没有签到账号");
  });

  it("retries a lost enable with the same resume handle", async () => {
    const resume = {
      op: "update_settings" as const,
      fingerprint: "f",
      requestID: "req_native_1",
    };
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: {
        ...readyStatus,
        enabled: false,
        storage_ready: false,
        scheduler_running: false,
      },
    });
    bridgeMocks.updateCheckinSettings
      .mockRejectedValueOnce(
        new CheckinBridgeError("transport", { retryable: true, resume }),
      )
      .mockResolvedValueOnce({ enabled: true });

    await render();
    await act(async () => button("开启签到").click());

    expect(container.querySelector('[role="alert"]')?.textContent).toBe(
      "网关没有响应，请重试。",
    );

    await act(async () => button("开启签到").click());

    expect(bridgeMocks.updateCheckinSettings).toHaveBeenLastCalledWith(
      true,
      resume,
    );
  });

  it("shows why an enabled extension is not ready", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: {
        ...readyStatus,
        storage_ready: false,
        scheduler_running: false,
        last_error_code: "init_failed",
      },
    });

    await render();

    expect(container.textContent).toContain("签到已开启但尚未就绪");
    expect(container.textContent).toContain("无法准备签到存储。");
    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
  });

  it("lists accounts and records in their own scrolling tab panels", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts.mockResolvedValue({
      items: [account(1), account(2)],
    });
    bridgeMocks.listCheckinJobs.mockResolvedValue({
      items: [finishedJob, uncertainJob],
    });

    await render();

    const panels = [
      ...container.querySelectorAll<HTMLElement>('[data-slot="tabs-content"]'),
    ];
    expect(panels).toHaveLength(2);
    for (const panel of panels) {
      expect(panel.className).toContain("min-h-0");
      expect(panel.className).toContain("overflow-y-auto");
      expect(panel.hasAttribute("data-tab-scroller")).toBe(true);
    }
    expect(
      container.querySelectorAll('[data-testid="checkin-account-row"]'),
    ).toHaveLength(2);
    expect(container.textContent).toContain("relay1.example");
    expect(container.textContent).toContain("已连接");
    expect(container.textContent).toContain("未连接");

    // Records stay mounted while hidden, so switching keeps scroll state.
    expect(
      container.querySelectorAll('[data-testid="checkin-job-row"]'),
    ).toHaveLength(2);
    expect(container.textContent).toContain("今日已完成");
    expect(container.textContent).toContain("站点未报告奖励");
    expect(container.textContent).toContain("结果未知");
    expect(container.textContent).toContain("签到请求已发送，但结果丢失");
  });

  it("pages long lists by cursor", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts
      .mockResolvedValueOnce({
        items: Array.from({ length: 20 }, (_, index) => account(index)),
        next_cursor: "cursor_2",
      })
      .mockResolvedValueOnce({ items: [account(20), account(21)] });

    await render();
    await act(async () => button("加载更多").click());

    expect(bridgeMocks.listCheckinAccounts).toHaveBeenLastCalledWith({
      cursor: "cursor_2",
    });
    expect(
      container.querySelectorAll('[data-testid="checkin-account-row"]'),
    ).toHaveLength(22);
    expect(
      [...document.querySelectorAll("button")].some(
        (node) => node.textContent?.trim() === "加载更多",
      ),
    ).toBe(false);
  });

  it("drops answers that arrive after the page is left", async () => {
    let finish: (value: unknown) => void = () => undefined;
    bridgeMocks.loadCheckinAvailability.mockReturnValue(
      new Promise((resolve) => {
        finish = resolve;
      }),
    );

    await render();
    await act(async () => root.unmount());
    root = createRoot(container);
    await act(async () => {
      finish({ kind: "present", status: readyStatus });
      await Promise.resolve();
    });

    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
  });

  it("does not read lists while a disabled extension is still retiring its storage", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: { ...readyStatus, enabled: false },
    });
    await render();
    expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
    expect(bridgeMocks.listCheckinJobs).not.toHaveBeenCalled();
  });

  it.each(["success", "failure"] as const)(
    "drops a late enable %s after unmount without another probe",
    async (outcome) => {
      const pending = deferred<{ enabled: boolean }>();
      bridgeMocks.loadCheckinAvailability.mockResolvedValue({
        kind: "present",
        status: disabledStatus,
      });
      bridgeMocks.updateCheckinSettings.mockReturnValue(pending.promise);
      await render();
      await act(async () => button("开启签到").click());
      await act(async () => root.unmount());
      root = createRoot(container);
      await act(async () => {
        if (outcome === "success") pending.resolve({ enabled: true });
        else pending.reject(new CheckinBridgeError("transport"));
      });
      expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(1);
      expect(bridgeMocks.listCheckinAccounts).not.toHaveBeenCalled();
      expect(container.textContent).toBe("");
    },
  );

  it("does not let an old Core's write end or overwrite the new Core's write", async () => {
    const oldWrite = deferred<{ enabled: boolean }>();
    const newWrite = deferred<{ enabled: boolean }>();
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: disabledStatus,
    });
    bridgeMocks.updateCheckinSettings
      .mockReturnValueOnce(oldWrite.promise)
      .mockReturnValueOnce(newWrite.promise);
    await render();
    await act(async () => button("开启签到").click());
    await render(true, "session-2");
    expect(button("开启签到").disabled).toBe(false);
    await act(async () => button("开启签到").click());
    await act(async () => oldWrite.reject(new CheckinBridgeError("transport")));
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(button("开启中…").disabled).toBe(true);
    expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(2);
    await act(async () => newWrite.resolve({ enabled: true }));
    expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(3);
  });

  it("does not refresh an offline Core when an earlier enable succeeds", async () => {
    const pending = deferred<{ enabled: boolean }>();
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: disabledStatus,
    });
    bridgeMocks.updateCheckinSettings.mockReturnValue(pending.promise);
    await render();
    await act(async () => button("开启签到").click());
    await render(false);
    await act(async () => pending.resolve({ enabled: true }));
    expect(container.textContent).toContain("等待本地网关");
    expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(1);
    await render();
    expect(button("开启签到").disabled).toBe(false);
    expect(bridgeMocks.loadCheckinAvailability).toHaveBeenCalledTimes(2);
  });

  it("admits only one enable before React commits the disabled button", async () => {
    const pending = deferred<{ enabled: boolean }>();
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: disabledStatus,
    });
    bridgeMocks.updateCheckinSettings.mockReturnValue(pending.promise);
    await render();
    await act(async () => {
      const enable = button("开启签到");
      enable.click();
      enable.click();
    });
    expect(bridgeMocks.updateCheckinSettings).toHaveBeenCalledTimes(1);
    await act(async () => pending.resolve({ enabled: true }));
  });

  it.each(["accounts", "records"] as const)(
    "isolates pending %s pages and their single-flight lock across Core sessions",
    async (tab) => {
      const oldPage = deferred<{ items: unknown[] }>();
      const newPage = deferred<{ items: unknown[] }>();
      const isAccounts = tab === "accounts";
      const list = isAccounts
        ? bridgeMocks.listCheckinAccounts
        : bridgeMocks.listCheckinJobs;
      const item = (index: number) =>
        isAccounts ? account(index) : { ...finishedJob, id: `job_${index}` };
      const selector = isAccounts
        ? '[data-testid="checkin-account-row"]'
        : '[data-testid="checkin-job-row"]';
      bridgeMocks.loadCheckinAvailability.mockResolvedValue({
        kind: "present",
        status: readyStatus,
      });
      list
        .mockResolvedValueOnce({ items: [item(1)], next_cursor: "old_cursor" })
        .mockReturnValueOnce(oldPage.promise)
        .mockResolvedValueOnce({ items: [item(9)], next_cursor: "new_cursor" })
        .mockReturnValueOnce(newPage.promise);
      await render();
      if (!isAccounts) await act(async () => button("记录").focus());
      await act(async () => {
        const more = button("加载更多");
        more.click();
        more.click();
      });
      expect(list).toHaveBeenCalledTimes(2);
      await render(true, "session-2");
      if (!isAccounts) await act(async () => button("记录").focus());
      await act(async () => button("加载更多").click());
      await act(async () => oldPage.resolve({ items: [item(2)] }));
      expect(button("加载中…").disabled).toBe(true);
      expect(container.querySelectorAll(selector)).toHaveLength(1);
      expect(list).toHaveBeenCalledTimes(4);
      await act(async () => newPage.resolve({ items: [item(10)] }));
      expect(container.querySelectorAll(selector)).toHaveLength(2);
      expect(list).toHaveBeenLastCalledWith({ cursor: "new_cursor" });
    },
  );

  it("dismisses a destructive confirmation when Core is replaced", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    await render();
    await act(async () => button("关闭签到").click());
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    await render(true, "session-2");
    expect(document.querySelector('[role="alertdialog"]')).toBeNull();
    expect(bridgeMocks.updateCheckinSettings).not.toHaveBeenCalled();
  });

  it("keeps a failed list read inside its tab", async () => {
    bridgeMocks.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: readyStatus,
    });
    bridgeMocks.listCheckinAccounts.mockRejectedValue(
      new CheckinBridgeError("rejected", { code: "checkin_storage_failed" }),
    );

    await render();

    expect(container.textContent).toContain(
      "签到存储出错，网关的其他功能不受影响。",
    );
    expect(container.textContent).toContain("还没有签到记录");
  });
});
