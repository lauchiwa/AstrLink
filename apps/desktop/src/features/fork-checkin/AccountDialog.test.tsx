// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const bridge = vi.hoisted(() => ({
  createCheckinAccount: vi.fn(),
  updateCheckinAccount: vi.fn(),
  deleteCheckinAccount: vi.fn(),
  getCheckinAccount: vi.fn(),
  beginCheckinAuthorization: vi.fn(),
}));
vi.mock("./bridge", async (original) => ({
  ...(await original<typeof import("./bridge")>()),
  ...bridge,
}));
import { AccountDialog } from "./AccountDialog";
import { DeleteAccountDialog } from "./DeleteAccountDialog";
import { CheckinBridgeError } from "./bridge";
import type { CheckinAccount } from "./model";

const base: CheckinAccount = {
  id: "acct_one",
  dashboard_base_url: "https://relay.example",
  state: "connected",
  revision: 2,
  network: { mode: "direct" },
  time_zone: "Asia/Shanghai",
  automatic: false,
  remote_user_id: "7",
  bound_services: ["svc_old"],
  config_fingerprint: "a".repeat(64),
};
const resume = {
  op: "create_account" as const,
  fingerprint: "draft",
  requestID: "req_native_one",
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

describe("check-in account management dialogs", () => {
  let root: Root;
  let container: HTMLDivElement;
  const onClose = vi.fn();
  const onSaved = vi.fn();
  const onDeleted = vi.fn();
  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.resetAllMocks();
    bridge.createCheckinAccount.mockResolvedValue({
      ...base,
      state: "draft",
      remote_user_id: undefined,
      bound_services: [],
    });
    bridge.updateCheckinAccount.mockResolvedValue({ ...base, revision: 3 });
    bridge.deleteCheckinAccount.mockResolvedValue(undefined);
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    document.body.innerHTML = "";
  });
  const render = async (
    account: CheckinAccount | null = base,
    services = [{ id: "svc_new", name: "New provider" }],
    servicesReady = true,
  ) => {
    await act(async () =>
      root.render(
        <AccountDialog
          account={account}
          services={services}
          servicesReady={servicesReady}
          onClose={onClose}
          onSaved={onSaved}
        />,
      ),
    );
  };
  const renderDelete = async () => {
    await act(async () =>
      root.render(
        <DeleteAccountDialog
          account={base}
          onClose={onClose}
          onDeleted={onDeleted}
        />,
      ),
    );
  };
  const button = (label: string) => {
    const node = [
      ...document.querySelectorAll<HTMLButtonElement>("button"),
    ].find(
      (item) =>
        (item.getAttribute("aria-label") ?? item.textContent?.trim()) === label,
    );
    if (!node) throw new Error(`missing button ${label}`);
    return node;
  };
  const click = async (label: string) => {
    await act(async () => button(label).click());
  };
  const input = (label: string) =>
    document.querySelector<HTMLInputElement>(`input[aria-label="${label}"]`)!;
  const fill = async (label: string, value: string) => {
    await act(async () => {
      Object.getOwnPropertyDescriptor(
        HTMLInputElement.prototype,
        "value",
      )!.set!.call(input(label), value);
      input(label).dispatchEvent(new Event("input", { bubbles: true }));
    });
  };
  const checkbox = (label: string) => {
    const node = [
      ...document.querySelectorAll<HTMLButtonElement>('[role="checkbox"]'),
    ].find(
      (item) =>
        item.getAttribute("aria-label") === label ||
        item.closest("label")?.textContent?.trim() === label,
    );
    if (!node) throw new Error(`missing checkbox ${label}`);
    return node;
  };
  const toggle = async (label: string) => {
    await act(async () => checkbox(label).click());
  };
  const target = () => toggle("我已核对后台地址与时区");

  it("keeps connection gated and cancellation makes no request", async () => {
    await render(null);
    expect(button("连接账号").disabled).toBe(true);
    expect(checkbox("每日自动签到").disabled).toBe(true);
    expect(document.querySelector('input[type="password"]')).toBeNull();
    await click("取消");
    expect(onClose).toHaveBeenCalledOnce();
    expect(bridge.createCheckinAccount).not.toHaveBeenCalled();
    expect(bridge.beginCheckinAuthorization).not.toHaveBeenCalled();
  });

  it("validates a confirmed draft and reuses the native id without a second create or binding patch", async () => {
    bridge.createCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume, retryable: true }),
    );
    await render(null);
    await click("添加账号");
    expect(document.body.textContent).toContain("请输入后台地址");
    await fill("后台地址", "https://relay.example");
    await fill("时区", "UTC");
    await click("添加账号");
    expect(document.body.textContent).toContain("请先核对并确认");
    await target();
    await act(async () => {
      button("添加账号").click();
      button("添加账号").click();
    });
    expect(bridge.createCheckinAccount).toHaveBeenCalledTimes(1);
    expect(onSaved).not.toHaveBeenCalled();
    await click("添加账号");
    expect(bridge.createCheckinAccount).toHaveBeenLastCalledWith(
      {
        dashboard_base_url: "https://relay.example",
        time_zone: "UTC",
        network: { mode: "direct" },
      },
      resume,
    );
    expect(bridge.updateCheckinAccount).not.toHaveBeenCalled();
    expect(onSaved).toHaveBeenCalledOnce();
  });

  it("asks separately for automation and does nothing when consent is declined", async () => {
    await render();
    await toggle("每日自动签到");
    await click("保存");
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    await click("暂不");
    expect(bridge.updateCheckinAccount).not.toHaveBeenCalled();
    await click("保存");
    await click("允许");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledWith(
      base.id,
      { expected_revision: 2, automatic: true },
      null,
    );
  });

  it("warns on a target change and sends automation off instead of reusing a login", async () => {
    await render({ ...base, automatic: true });
    await fill("后台地址", "https://other.example");
    expect(document.body.textContent).toContain("清除该账号已保存的登录");
    expect(checkbox("每日自动签到").disabled).toBe(true);
    await target();
    await fill("时区", "UTC");
    expect(
      checkbox("我已核对后台地址与时区").getAttribute("aria-checked"),
    ).toBe("false");
    await target();
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledWith(
      base.id,
      {
        expected_revision: 2,
        dashboard_base_url: "https://other.example",
        time_zone: "UTC",
        automatic: false,
      },
      null,
    );
  });

  it("keeps edits on CAS failure and reloads only on an explicit request", async () => {
    bridge.updateCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("rejected", { code: "revision_conflict" }),
    );
    bridge.getCheckinAccount.mockResolvedValue({
      ...base,
      revision: 8,
      time_zone: "Europe/Berlin",
    });
    await render();
    await fill("时区", "UTC");
    await target();
    await click("保存");
    expect(input("时区").value).toBe("UTC");
    expect(button("保存").disabled).toBe(true);
    expect(bridge.getCheckinAccount).not.toHaveBeenCalled();
    await click("重新载入账号");
    expect(input("时区").value).toBe("Europe/Berlin");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledTimes(1);
    await fill("时区", "Asia/Tokyo");
    await target();
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenLastCalledWith(
      base.id,
      { expected_revision: 8, time_zone: "Asia/Tokyo" },
      null,
    );
  });

  it("preserves bindings when the service catalog is unavailable", async () => {
    await render(base, [], false);
    expect(button("关联提供商").disabled).toBe(true);
    await fill("时区", "UTC");
    await target();
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledWith(
      base.id,
      { expected_revision: 2, time_zone: "UTC" },
      null,
    );
    expect(base.bound_services).toEqual(["svc_old"]);
  });

  it("keeps removed bindings visible and uses provider ids rather than names", async () => {
    const services = [{ id: "svc_new", name: "New provider" }];
    const original = JSON.stringify(services);
    await render(base, services);
    await click("关联提供商");
    expect(checkbox("svc_old（已删除）").getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(
      document.querySelector('[data-slot="popover-content"]')?.className,
    ).toContain("z-110");
    await toggle("New provider");
    await render(base, [{ id: "svc_new", name: "Renamed provider" }]);
    expect(checkbox("Renamed provider").getAttribute("aria-checked")).toBe(
      "true",
    );
    await click("关联提供商");
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledWith(
      base.id,
      { expected_revision: 2, bound_services: ["svc_new", "svc_old"] },
      null,
    );
    expect(JSON.stringify(services)).toBe(original);
  });

  it("does not save a newly selected provider deleted before saving", async () => {
    await render();
    await click("关联提供商");
    await toggle("New provider");
    await click("关联提供商");
    await render(base, []);
    await click("保存");
    expect(document.body.textContent).toContain("选中的提供商已被删除");
    expect(bridge.updateCheckinAccount).not.toHaveBeenCalled();
    await click("关联提供商");
    expect(checkbox("svc_new（已删除）")).not.toBeNull();
    await toggle("svc_new（已删除）");
    await click("关联提供商");
    await click("保存");
    expect(document.body.textContent).toContain("没有需要保存的修改");
  });

  it("bounds bindings to 32 even when the shared selector selects all", async () => {
    await render(
      { ...base, bound_services: [] },
      Array.from({ length: 33 }, (_, index) => ({
        id: `svc_${index}`,
        name: `Provider ${index}`,
      })),
    );
    await click("关联提供商");
    await click("全选");
    await click("关联提供商");
    await click("保存");
    expect(document.body.textContent).toContain("最多关联 32 个提供商");
    expect(bridge.updateCheckinAccount).not.toHaveBeenCalled();
  });

  it("drops pending writes after unmount without notifying a new workspace", async () => {
    const pending = deferred<CheckinAccount>();
    bridge.updateCheckinAccount.mockReturnValue(pending.promise);
    await render();
    await fill("时区", "UTC");
    await target();
    await click("保存");
    expect(button("取消").disabled).toBe(true);
    await act(async () =>
      document.activeElement?.dispatchEvent(
        new KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
      ),
    );
    expect(onClose).not.toHaveBeenCalled();
    await act(async () => root.unmount());
    root = createRoot(container);
    await act(async () => pending.resolve(base));
    expect(onSaved).not.toHaveBeenCalled();
    expect(bridge.getCheckinAccount).not.toHaveBeenCalled();
  });

  it("requires deletion confirmation and preserves the retry handle", async () => {
    const lost = { ...resume, op: "delete_account" as const };
    bridge.deleteCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume: lost, retryable: true }),
    );
    await renderDelete();
    expect(bridge.deleteCheckinAccount).not.toHaveBeenCalled();
    await click("删除");
    expect(onDeleted).not.toHaveBeenCalled();
    await click("删除");
    expect(bridge.deleteCheckinAccount).toHaveBeenLastCalledWith(
      base.id,
      base.revision,
      lost,
    );
    expect(onDeleted).toHaveBeenCalledWith(base.id);
  });

  it("does not delete a new revision without another explicit confirmation", async () => {
    bridge.deleteCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("rejected", { code: "revision_conflict" }),
    );
    bridge.getCheckinAccount.mockResolvedValue({ ...base, revision: 9 });
    await renderDelete();
    await click("删除");
    expect(button("删除").disabled).toBe(true);
    await click("重新载入账号");
    expect(bridge.deleteCheckinAccount).toHaveBeenCalledTimes(1);
    await click("删除");
    expect(bridge.deleteCheckinAccount).toHaveBeenLastCalledWith(
      base.id,
      9,
      null,
    );
  });

  it("reuses the update receipt after a lost response and serializes same-tick saves", async () => {
    const lost = { ...resume, op: "update_account" as const };
    bridge.updateCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume: lost, retryable: true }),
    );
    await render();
    await fill("时区", "UTC");
    await target();
    await act(async () => {
      button("保存").click();
      button("保存").click();
      button("取消").click();
    });
    expect(bridge.updateCheckinAccount).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenLastCalledWith(
      base.id,
      { expected_revision: 2, time_zone: "UTC" },
      lost,
    );
    expect(onSaved).toHaveBeenCalledOnce();
  });

  it("freezes changed bindings during a catalog outage without losing the selection", async () => {
    await render();
    await click("关联提供商");
    await toggle("New provider");
    await render(base, [], false);
    expect(button("关联提供商").disabled).toBe(true);
    expect(document.querySelector('[data-slot="popover-content"]')).toBeNull();
    await click("保存");
    expect(document.body.textContent).toContain("提供商列表尚未就绪");
    expect(bridge.updateCheckinAccount).not.toHaveBeenCalled();
    await render();
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledWith(
      base.id,
      { expected_revision: 2, bound_services: ["svc_new", "svc_old"] },
      null,
    );
  });

  it("keeps CAS blocked after a failed reload and requires fresh automatic consent", async () => {
    bridge.updateCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("rejected", { code: "revision_conflict" }),
    );
    bridge.getCheckinAccount
      .mockRejectedValueOnce(new CheckinBridgeError("transport"))
      .mockResolvedValueOnce({ ...base, revision: 9 });
    await render();
    await toggle("每日自动签到");
    await click("保存");
    await click("允许");
    expect(button("保存").disabled).toBe(true);
    await click("重新载入账号");
    expect(button("保存").disabled).toBe(true);
    expect(checkbox("每日自动签到").getAttribute("aria-checked")).toBe("true");
    await click("重新载入账号");
    expect(checkbox("每日自动签到").getAttribute("aria-checked")).toBe("false");
    await toggle("每日自动签到");
    await click("保存");
    expect(bridge.updateCheckinAccount).toHaveBeenCalledTimes(1);
    await click("允许");
    expect(bridge.updateCheckinAccount).toHaveBeenLastCalledWith(
      base.id,
      { expected_revision: 9, automatic: true },
      null,
    );
  });

  it("never recreates an account removed while it was being edited", async () => {
    bridge.updateCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("rejected", { code: "not_found" }),
    );
    await render();
    await fill("时区", "UTC");
    await target();
    await click("保存");
    expect(button("保存").disabled).toBe(true);
    expect(input("时区").value).toBe("UTC");
    expect(bridge.createCheckinAccount).not.toHaveBeenCalled();
  });

  it.each(["success", "failure"] as const)(
    "drops a pending reload %s after the editor is unmounted",
    async (outcome) => {
      const pending = deferred<CheckinAccount>();
      bridge.updateCheckinAccount.mockRejectedValueOnce(
        new CheckinBridgeError("rejected", { code: "revision_conflict" }),
      );
      bridge.getCheckinAccount.mockReturnValue(pending.promise);
      await render();
      await fill("时区", "UTC");
      await target();
      await click("保存");
      await click("重新载入账号");
      await act(async () => root.unmount());
      root = createRoot(container);
      await act(async () => {
        if (outcome === "success") pending.resolve({ ...base, revision: 9 });
        else pending.reject(new CheckinBridgeError("transport"));
      });
      expect(onSaved).not.toHaveBeenCalled();
      expect(onClose).not.toHaveBeenCalled();
      expect(bridge.getCheckinAccount).toHaveBeenCalledTimes(1);
      expect(bridge.updateCheckinAccount).toHaveBeenCalledTimes(1);
      expect(container.textContent).toBe("");
    },
  );

  it("keeps deletion blocked after a failed CAS reload and handles an already removed account", async () => {
    bridge.deleteCheckinAccount.mockRejectedValueOnce(
      new CheckinBridgeError("rejected", { code: "revision_conflict" }),
    );
    bridge.getCheckinAccount
      .mockRejectedValueOnce(new CheckinBridgeError("transport"))
      .mockRejectedValueOnce(
        new CheckinBridgeError("rejected", { code: "not_found" }),
      );
    await renderDelete();
    await click("删除");
    await click("重新载入账号");
    expect(button("删除").disabled).toBe(true);
    expect(onDeleted).not.toHaveBeenCalled();
    await click("重新载入账号");
    expect(bridge.deleteCheckinAccount).toHaveBeenCalledTimes(1);
    expect(onDeleted).toHaveBeenCalledWith(base.id);
  });

  it("serializes same-tick deletions and ignores completion after unmount", async () => {
    const pending = deferred<void>();
    bridge.deleteCheckinAccount.mockReturnValue(pending.promise);
    await renderDelete();
    await act(async () => {
      button("删除").click();
      button("删除").click();
      button("保留").click();
    });
    expect(bridge.deleteCheckinAccount).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
    await act(async () => root.unmount());
    root = createRoot(container);
    await act(async () => pending.resolve());
    expect(onDeleted).not.toHaveBeenCalled();
  });

  it("keeps a cancelled delete local", async () => {
    await renderDelete();
    await click("保留");
    expect(onClose).toHaveBeenCalledOnce();
    expect(bridge.deleteCheckinAccount).not.toHaveBeenCalled();
  });
});
