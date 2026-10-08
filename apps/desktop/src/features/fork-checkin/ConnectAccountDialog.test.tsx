// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const bridge = vi.hoisted(() => ({
  beginCheckinAuthorization: vi.fn(),
  completeCheckinAuthorization: vi.fn(),
  getCheckinAccount: vi.fn(),
}));
vi.mock("./bridge", async (original) => ({
  ...(await original<typeof import("./bridge")>()),
  ...bridge,
}));
import { CheckinBridgeError } from "./bridge";
import { ConnectAccountDialog } from "./ConnectAccountDialog";
import type { CheckinAccount, CheckinAuthorization } from "./model";

const draft: CheckinAccount = {
  id: "acct_one",
  dashboard_base_url: "https://relay.example",
  state: "draft",
  revision: 2,
  network: { mode: "direct" },
  time_zone: "Asia/Shanghai",
  automatic: false,
  bound_services: [],
  config_fingerprint: "a".repeat(64),
};
const session: CheckinAuthorization = {
  session_id: "session_0001",
  account_id: draft.id,
  expires_at: "2099-01-01T00:00:00Z",
  config_fingerprint: "a".repeat(64),
  login_url: "https://relay.example/login",
};
const PASTE = "session=pastedsecretvalue; csrf=second";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

describe("check-in paste connect dialog", () => {
  let root: Root;
  let container: HTMLDivElement;
  const onClose = vi.fn();
  const onConnected = vi.fn();

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.resetAllMocks();
    bridge.beginCheckinAuthorization.mockResolvedValue(session);
    bridge.completeCheckinAuthorization.mockResolvedValue({
      ...draft,
      state: "connected",
      remote_user_id: "7",
      revision: 3,
    });
    localStorage.clear();
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    document.body.innerHTML = "";
  });

  const render = async (account: CheckinAccount = draft) => {
    await act(async () =>
      root.render(
        <ConnectAccountDialog
          account={account}
          onClose={onClose}
          onConnected={onConnected}
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
  const area = () =>
    document.querySelector<HTMLTextAreaElement>(
      "#checkin-connect-paste",
    ) as HTMLTextAreaElement;
  const paste = async (value: string) => {
    await act(async () => {
      Object.getOwnPropertyDescriptor(
        HTMLTextAreaElement.prototype,
        "value",
      )!.set!.call(area(), value);
      area().dispatchEvent(new Event("input", { bubbles: true }));
    });
  };
  const text = () => document.body.textContent ?? "";

  it("begins one session and offers the site link for the person's own browser", async () => {
    await render();
    expect(bridge.beginCheckinAuthorization).toHaveBeenCalledTimes(1);
    expect(bridge.beginCheckinAuthorization).toHaveBeenCalledWith(
      "acct_one",
      2,
    );
    // The login page is opened by the operator, not loaded in this app.
    const link = document.querySelector<HTMLAnchorElement>(
      'a[href="https://relay.example/login"]',
    );
    expect(link).not.toBeNull();
    // The shared link component hands the URL to the OS browser; the login
    // page is never loaded inside the app.
    expect(document.querySelectorAll("iframe, webview")).toHaveLength(0);
  });

  it("hands the paste to the host and keeps no copy of it", async () => {
    await render();
    await paste(PASTE);
    await click("连接");
    expect(bridge.completeCheckinAuthorization).toHaveBeenCalledWith(
      {
        session_id: "session_0001",
        pasted_cookies: PASTE,
        dashboard_base_url: "https://relay.example",
        expected_revision: 2,
      },
      null,
    );
    expect(onConnected).toHaveBeenCalledWith(
      expect.objectContaining({ state: "connected", remote_user_id: "7" }),
    );
    // Nothing of the session survives in the dialog, the DOM or storage.
    expect(area().value).toBe("");
    expect(text()).not.toContain("pastedsecretvalue");
    expect(container.innerHTML).not.toContain("pastedsecretvalue");
    expect(JSON.stringify(localStorage)).not.toContain("pastedsecretvalue");
  });

  it("clears the paste before the host answers, not after", async () => {
    const pending = deferred<CheckinAccount>();
    bridge.completeCheckinAuthorization.mockReturnValue(pending.promise);
    await render();
    await paste(PASTE);
    await click("连接");
    // While the host is still converting, the window holds no copy.
    expect(area().value).toBe("");
    expect(container.innerHTML).not.toContain("pastedsecretvalue");
    expect(button("连接中…").disabled).toBe(true);
    await act(async () => {
      pending.resolve({ ...draft, state: "connected", remote_user_id: "7" });
      await pending.promise;
    });
  });

  it("refuses to submit nothing and cannot submit twice", async () => {
    const pending = deferred<CheckinAccount>();
    bridge.completeCheckinAuthorization.mockReturnValue(pending.promise);
    await render();
    expect(button("连接").disabled).toBe(true);
    await paste("   ");
    expect(button("连接").disabled).toBe(true);
    await paste(PASTE);
    expect(button("连接").disabled).toBe(false);
    await click("连接");
    await click("连接中…");
    expect(bridge.completeCheckinAuthorization).toHaveBeenCalledTimes(1);
    await act(async () => {
      pending.resolve({ ...draft, state: "connected" });
      await pending.promise;
    });
  });

  it("shows each refusal Core can give and never echoes the session", async () => {
    const cases: [string, string, boolean][] = [
      ["identity_mismatch", "未保存任何内容", false],
      ["auth_required", "站点未接受该会话", false],
      ["manual_required", "站点要求先在其页面", false],
      ["unsupported", "无法以本版本支持的方式", false],
      ["authorization_not_found", "登录会话已过期", false],
      ["authorization_completed", "已经使用过", false],
      ["request_id_reused", "", false],
      ["payload_too_large", "", false],
      ["site_unavailable", "", true],
      ["rate_limited", "", true],
      ["account_busy", "", true],
    ];
    for (const [code, fragment, retryable] of cases) {
      vi.resetAllMocks();
      bridge.beginCheckinAuthorization.mockResolvedValue(session);
      bridge.completeCheckinAuthorization.mockRejectedValue(
        new CheckinBridgeError("rejected", { code, retryable }),
      );
      await render();
      await paste(PASTE);
      await click("连接");
      const shown = text();
      if (fragment) expect(shown, code).toContain(fragment);
      expect(shown, code).not.toContain("pastedsecretvalue");
      expect(onConnected, code).not.toHaveBeenCalled();
      // Only Core's own retryable flag offers a retry.
      expect(shown.includes("重新粘贴会话后重试"), code).toBe(retryable);
      // A session the site will never accept again must be restarted, not
      // resubmitted; the textarea stays closed until it is.
      const restart = [
        "revision_conflict",
        "authorization_not_found",
        "authorization_completed",
      ].includes(code);
      expect(shown.includes("重新载入账号"), code).toBe(restart);
      expect(area().disabled, code).toBe(restart);
      await act(async () => root.render(<div />));
    }
  });

  it("blocks submitting after a revision conflict until the account is reloaded", async () => {
    bridge.completeCheckinAuthorization.mockRejectedValue(
      new CheckinBridgeError("rejected", { code: "revision_conflict" }),
    );
    bridge.getCheckinAccount.mockResolvedValue({ ...draft, revision: 5 });
    await render();
    await paste(PASTE);
    await click("连接");
    expect(area().disabled).toBe(true);
    expect(button("连接").disabled).toBe(true);
    await click("重新载入账号");
    // The restart reads the account again and begins a session bound to the
    // revision it actually has now.
    expect(bridge.getCheckinAccount).toHaveBeenCalledWith("acct_one");
    expect(bridge.beginCheckinAuthorization).toHaveBeenLastCalledWith(
      "acct_one",
      5,
    );
    expect(area().disabled).toBe(false);
  });

  it("replays the same request after a lost answer instead of acting twice", async () => {
    const resume = {
      op: "complete_authorization" as const,
      fingerprint: "session_0001",
      requestID: "req_native_one",
    };
    bridge.completeCheckinAuthorization.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { retryable: true, resume }),
    );
    await render();
    await paste(PASTE);
    await click("连接");
    expect(text()).toContain("重新粘贴会话后重试");
    // The operator pastes again; the id Core already issued is reused.
    await paste(PASTE);
    await click("连接");
    expect(bridge.completeCheckinAuthorization).toHaveBeenLastCalledWith(
      expect.objectContaining({ session_id: "session_0001" }),
      resume,
    );
  });
});
