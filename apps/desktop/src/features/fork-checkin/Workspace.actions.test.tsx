// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const bridge = vi.hoisted(() => ({
  loadCheckinAvailability: vi.fn(),
  listCheckinAccounts: vi.fn(),
  listCheckinJobs: vi.fn(),
  getCheckinAccount: vi.fn(),
  createCheckinJob: vi.fn(),
  getCheckinJob: vi.fn(),
  cancelCheckinJob: vi.fn(),
  beginCheckinAuthorization: vi.fn(),
}));
vi.mock("./bridge", async (original) => ({
  ...(await original<typeof import("./bridge")>()),
  ...bridge,
}));
import { CheckinBridgeError } from "./bridge";
import type { CheckinAccount, CheckinJob } from "./model";
import { MAX_POLL_REQUESTS, POLL_INTERVAL_MS } from "./use-checkin-actions";
import { CheckinWorkspace } from "./Workspace";

function account(index = 1): CheckinAccount {
  return {
    id: `acct_${index}`,
    dashboard_base_url: `https://relay${index}.example`,
    state: "connected",
    revision: 1,
    time_zone: "UTC",
    network: { mode: "direct" },
    automatic: false,
    remote_user_id: String(index),
    bound_services: [],
    config_fingerprint: "a".repeat(64),
  };
}
function queued(index = 1): CheckinJob {
  return {
    id: `job_${index}`,
    account_id: `acct_${index}`,
    action: "check_in",
    status: "queued",
    dispatched: false,
    proof_source: "none",
    created_at: new Date(index * 1000).toISOString(),
    children: [],
  };
}
const success = (index = 1): CheckinJob => ({
  ...queued(index),
  status: "success",
  dispatched: true,
  proof_source: "submission_response",
  finished_at: new Date(100_000).toISOString(),
  reward: { known: false },
});
const parent = (children: CheckinJob[]): CheckinJob => ({
  ...queued(),
  id: "batch_one",
  account_id: undefined,
  status: "running",
  children,
});
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((yes) => {
    resolve = yes;
  });
  return { promise, resolve };
}

describe("check-in task workspace", () => {
  let root: Root;
  let container: HTMLDivElement;
  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.resetAllMocks();
    bridge.loadCheckinAvailability.mockResolvedValue({
      kind: "present",
      status: {
        protocol_version: 1,
        present: true,
        enabled: true,
        storage_ready: true,
        scheduler_running: true,
      },
    });
    bridge.listCheckinAccounts.mockResolvedValue({ items: [account()] });
    bridge.listCheckinJobs.mockResolvedValue({ items: [] });
    bridge.createCheckinJob.mockResolvedValue(queued());
    bridge.getCheckinAccount.mockImplementation(async (id: string) =>
      account(Number(id.split("_")[1])),
    );
    bridge.getCheckinJob.mockResolvedValue(queued());
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    document.body.innerHTML = "";
    vi.useRealTimers();
  });
  const render = async (isReady = true, session = "core-one") => {
    await act(async () =>
      root.render(
        <CheckinWorkspace isReady={isReady} coreSessionKey={session} />,
      ),
    );
  };
  const button = (name: string, scope: ParentNode = container) => {
    const node = [...scope.querySelectorAll<HTMLButtonElement>("button")].find(
      (item) =>
        (item.getAttribute("aria-label") ?? item.textContent?.trim()) === name,
    );
    if (!node) throw new Error(`missing ${name}`);
    return node;
  };
  const click = async (name: string, scope?: ParentNode) => {
    await act(async () => button(name, scope).click());
  };
  const rows = () => [
    ...container.querySelectorAll<HTMLElement>(
      '[data-testid="checkin-account-row"]',
    ),
  ];
  const tick = async () => {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
    });
  };
  const recordsTab = async () => {
    await act(async () => button("记录").focus());
  };

  it("starts a single job, shows its authoritative result and refreshes only that account", async () => {
    vi.useFakeTimers();
    bridge.getCheckinJob.mockResolvedValue(success());
    await render();
    await click("签到", rows()[0]);
    expect(bridge.createCheckinJob).toHaveBeenCalledWith(
      { action: "check_in", accounts: ["acct_1"], expected_revision: 1 },
      null,
    );
    expect(rows()[0].textContent).toContain("排队中");
    expect(button("签到", rows()[0]).disabled).toBe(true);
    expect(button("编辑 relay1.example", rows()[0]).disabled).toBe(true);
    await tick();
    expect(rows()[0].textContent).toContain("今日已完成");
    expect(rows()[0].textContent).toContain("站点未报告奖励");
    expect(bridge.getCheckinAccount).toHaveBeenCalledWith("acct_1");
    expect(bridge.listCheckinAccounts).toHaveBeenCalledOnce();
    expect(bridge.loadCheckinAvailability).toHaveBeenCalledOnce();
    await tick();
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
  });

  it("selects at most 25 and preserves individual batch outcomes inside the scroller", async () => {
    const accounts = Array.from({ length: 30 }, (_, index) =>
      account(index + 1),
    );
    bridge.listCheckinAccounts.mockResolvedValue({ items: accounts });
    bridge.createCheckinJob.mockResolvedValue(
      parent([
        success(1),
        { ...queued(2), status: "auth_required" },
        { ...queued(3), status: "uncertain", dispatched: true },
        ...accounts.slice(3, 25).map((_, index) => queued(index + 4)),
      ]),
    );
    await render();
    await click("选择可操作账号");
    expect(
      container.querySelector('[data-testid="checkin-batch-toolbar"]')
        ?.textContent,
    ).toContain("已选 25 / 25");
    await click("签到所选");
    expect(bridge.createCheckinJob.mock.calls[0][0].accounts).toHaveLength(25);
    expect(bridge.createCheckinJob.mock.calls[0][0]).not.toHaveProperty(
      "expected_revision",
    );
    expect(rows()[0].textContent).toContain("今日已完成");
    expect(rows()[1].textContent).toContain("需要你处理");
    expect(rows()[2].textContent).toContain("结果未知");
    expect(button("签到", rows()[2]).disabled).toBe(true);
    expect(button("刷新 relay3.example 的状态", rows()[2]).disabled).toBe(
      false,
    );
    expect(
      container
        .querySelector('[data-testid="checkin-batch-summary"]')
        ?.closest("[data-tab-scroller]"),
    ).not.toBeNull();
    expect(
      container.querySelector('[data-testid="checkin-batch-toolbar"]')
        ?.textContent,
    ).toContain("已选 0 / 25");
  });

  it("retries a lost batch receipt without requiring the original selection again", async () => {
    const resume = {
      op: "create_job" as const,
      requestID: "req_batch_retry",
      fingerprint: "fixed",
    };
    bridge.listCheckinAccounts.mockResolvedValue({
      items: [account(1), account(2)],
    });
    bridge.createCheckinJob
      .mockRejectedValueOnce(new CheckinBridgeError("transport", { resume }))
      .mockResolvedValueOnce(parent([queued(1), queued(2)]));
    await render();
    await click("选择可操作账号");
    await click("签到所选");
    const summary = container.querySelector(
      '[data-testid="checkin-batch-summary"]',
    )!;
    await click("重试原操作", summary);
    expect(bridge.createCheckinJob).toHaveBeenLastCalledWith(
      { action: "check_in", accounts: ["acct_1", "acct_2"] },
      resume,
    );
  });

  it("offers cancellation for persisted jobs and does not promise rollback after dispatch", async () => {
    bridge.listCheckinJobs.mockResolvedValue({ items: [queued()] });
    bridge.cancelCheckinJob.mockResolvedValue({
      ...queued(),
      status: "running",
      dispatched: true,
    });
    await render();
    await recordsTab();
    const row = container.querySelector('[data-testid="checkin-job-row"]')!;
    await click("取消", row);
    expect(bridge.cancelCheckinJob).toHaveBeenCalledWith("job_1", null);
    expect(row.textContent).toContain("取消不会撤回");
    expect(row.textContent).toContain("已发送");
    expect(
      [...row.querySelectorAll("button")].some(
        (item) => item.textContent === "取消",
      ),
    ).toBe(false);
    expect(bridge.createCheckinJob).not.toHaveBeenCalled();
  });

  it("offers only a status refresh for a persisted cancellation after dispatch", async () => {
    bridge.listCheckinJobs.mockResolvedValue({
      items: [{ ...queued(), status: "cancelled", dispatched: true }],
    });
    await render();
    expect(button("签到", rows()[0]).disabled).toBe(true);
    await recordsTab();
    const row = container.querySelector('[data-testid="checkin-job-row"]')!;
    await click("刷新状态", row);
    expect(bridge.createCheckinJob.mock.calls[0][0].action).toBe(
      "status_refresh",
    );
    expect(bridge.cancelCheckinJob).not.toHaveBeenCalled();
  });

  it("keeps a manual step gated while allowing an explicit read-only status task", async () => {
    bridge.listCheckinAccounts.mockResolvedValue({
      items: [{ ...account(), state: "manual_required" }],
    });
    await render();
    expect(button("到站点处理", rows()[0]).disabled).toBe(true);
    await click("刷新 relay1.example 的状态", rows()[0]);
    expect(bridge.createCheckinJob.mock.calls[0][0].action).toBe(
      "status_refresh",
    );
    expect(bridge.beginCheckinAuthorization).not.toHaveBeenCalled();
  });

  it("removes a pruned persisted task rather than leaving a permanently pending row", async () => {
    vi.useFakeTimers();
    bridge.listCheckinJobs.mockResolvedValue({ items: [queued()] });
    bridge.getCheckinJob.mockRejectedValue(
      new CheckinBridgeError("rejected", { code: "not_found" }),
    );
    await render();
    await tick();
    await recordsTab();
    expect(
      container.querySelectorAll('[data-testid="checkin-job-row"]'),
    ).toHaveLength(0);
    expect(container.textContent).toContain("还没有签到记录");
    await tick();
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
  });

  it("does not refresh accounts when a create result arrives after leaving the page", async () => {
    const pending = deferred<CheckinJob>();
    bridge.createCheckinJob.mockReturnValue(pending.promise);
    await render();
    await click("签到", rows()[0]);
    await act(async () => root.unmount());
    root = createRoot(container);
    await act(async () => pending.resolve(success()));
    expect(bridge.getCheckinAccount).not.toHaveBeenCalled();
    expect(bridge.listCheckinJobs).toHaveBeenCalledOnce();
  });

  it("bounds targeted reads across overlapping settlements and stops draining when offline", async () => {
    vi.useFakeTimers();
    const pending = deferred<CheckinAccount>();
    const initial = parent(Array.from({ length: 9 }, (_, i) => queued(i + 1)));
    bridge.listCheckinAccounts.mockResolvedValue({
      items: Array.from({ length: 10 }, (_, i) => account(i + 1)),
    });
    bridge.createCheckinJob
      .mockResolvedValueOnce(initial)
      .mockResolvedValueOnce(success(10));
    bridge.getCheckinJob.mockResolvedValue({
      ...initial,
      status: "success",
      dispatched: true,
      children: initial.children.map((_, i) => success(i + 1)),
    });
    bridge.getCheckinAccount.mockReturnValue(pending.promise);
    await render();
    await click("选择可操作账号");
    await click("选择 relay10.example");
    await click("签到所选");
    await tick();
    expect(bridge.getCheckinAccount).toHaveBeenCalledTimes(MAX_POLL_REQUESTS);
    await click("签到", rows()[9]);
    expect(bridge.getCheckinAccount).toHaveBeenCalledTimes(MAX_POLL_REQUESTS);
    await render(false);
    await act(async () => pending.resolve(account(1)));
    expect(bridge.getCheckinAccount).toHaveBeenCalledTimes(MAX_POLL_REQUESTS);
  });

  it("ignores a targeted read whose account id does not match the request", async () => {
    bridge.listCheckinAccounts.mockResolvedValue({
      items: [account(1), account(2)],
    });
    bridge.createCheckinJob.mockResolvedValue(success());
    bridge.getCheckinAccount.mockResolvedValue({
      ...account(2),
      revision: 5,
      state: "auth_required",
    });
    await render();
    await click("签到", rows()[0]);
    expect(rows()[1].textContent).toContain("已连接");
    expect(button("签到", rows()[1]).disabled).toBe(false);
  });

  it("drops a targeted account refresh from an old Core", async () => {
    const pending = deferred<CheckinAccount>();
    bridge.createCheckinJob.mockResolvedValue(success());
    bridge.getCheckinAccount.mockReturnValue(pending.promise);
    await render();
    await click("签到", rows()[0]);
    bridge.listCheckinAccounts.mockResolvedValue({ items: [account(2)] });
    await render(true, "core-two");
    await act(async () =>
      pending.resolve({ ...account(), state: "auth_required", revision: 5 }),
    );
    expect(container.textContent).toContain("relay2.example");
    expect(container.textContent).not.toContain("relay1.example");
    expect(bridge.getCheckinAccount).toHaveBeenCalledOnce();
  });
});
