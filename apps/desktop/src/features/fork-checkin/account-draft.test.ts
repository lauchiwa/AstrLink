import { describe, expect, it } from "vitest";
import {
  accountDraft,
  createAccountInput,
  dashboardError,
  draftError,
  redirectsAccount,
  timeZoneError,
  updateAccountInput,
} from "./account-draft";
import type { CheckinAccount } from "./model";

const account: CheckinAccount = {
  id: "acct_one",
  dashboard_base_url: "https://relay.example",
  state: "connected",
  revision: 3,
  network: { mode: "direct" },
  time_zone: "Asia/Shanghai",
  automatic: true,
  remote_user_id: "7",
  bound_services: ["svc_one"],
  config_fingerprint: "a".repeat(64),
};

describe("check-in account drafts", () => {
  it("starts with a manual draft and creates exactly one draft payload", () => {
    const draft = accountDraft(null);
    expect(draft.networkMode).toBe("direct");
    expect(draft.automatic).toBe(false);
    expect(draft.targetConfirmed).toBe(false);
    draft.dashboardURL = "https://relay.example";
    draft.timeZone = "UTC";
    draft.boundServices = ["svc_one"];
    draft.automatic = true;
    expect(createAccountInput(draft)).toEqual({
      dashboard_base_url: "https://relay.example",
      time_zone: "UTC",
      network: { mode: "direct" },
    });
    expect(draftError(draft, null)).toBe("confirmRequired");
    draft.targetConfirmed = true;
    expect(draftError(draft, null)).toBeNull();
  });

  it.each([
    "https://relay.example",
    "https://relay.example/console/",
    "http://localhost:8080",
    "http://127.0.0.2:8080",
    "http://[::1]:8080",
  ])("accepts %s", (url) => {
    expect(dashboardError(url)).toBeNull();
  });

  it.each([
    "",
    "file:///tmp/site",
    "https://user:pw@relay.example",
    "https://relay.example?",
    "https://relay.example#fragment",
    "http://relay.example",
    "http://127.1",
    "http://2130706433",
    "https://relay.example:0",
    "https://relay.example:",
    " https://relay.example",
    "https://relay.example\\path",
    "https://relay.example/a/../b",
    "https://relay.example/%2e%2e/a",
    "https://relay.example/a//b",
    "https://relay.example/a%2fb",
    "https://relay.example/%25data",
    "https://relay.example/%00",
  ])("rejects an unsafe or repaired URL: %s", (url) => {
    expect(dashboardError(url)).not.toBeNull();
  });

  it("requires an explicit IANA zone and an address-only custom proxy", () => {
    for (const zone of [
      "UTC",
      "Asia/Shanghai",
      "America/New_York",
      "Etc/GMT+8",
    ])
      expect(timeZoneError(zone)).toBeNull();
    for (const zone of ["", "Local", "+08:00", "Unknown/Zone", " UTC"])
      expect(timeZoneError(zone)).not.toBeNull();
    const draft = {
      ...accountDraft(account),
      networkMode: "custom" as const,
      targetConfirmed: true,
    };
    expect(draftError(draft, account)).toBe("proxyRequired");
    for (const proxyURL of [
      "http://localhost:8080",
      "https://proxy.example:443",
      "socks5://127.0.0.1:1080",
    ]) {
      expect(draftError({ ...draft, proxyURL }, account)).toBeNull();
    }
    expect(
      draftError(
        { ...draft, proxyURL: "socks5://u:p@proxy.example:1080" },
        account,
      ),
    ).toBe("proxyInvalid");
  });

  it("makes a minimal CAS patch and does not mutate bound provider data", () => {
    const draft = accountDraft(account);
    expect(updateAccountInput(draft, account)).toBeNull();
    draft.boundServices.push("svc_two");
    expect(updateAccountInput(draft, account)).toEqual({
      expected_revision: 3,
      bound_services: ["svc_one", "svc_two"],
    });
    expect(account.bound_services).toEqual(["svc_one"]);
    expect(redirectsAccount(draft, account)).toBe(false);
    expect(draftError(draft, account)).toBeNull();
    draft.boundServices = Array.from(
      { length: 33 },
      (_, index) => `svc_${index}`,
    );
    expect(draftError(draft, account)).toBe("servicesLimit");
  });

  it("requires target confirmation again and turns automation off for redirected accounts", () => {
    const draft = {
      ...accountDraft(account),
      dashboardURL: "https://other.example",
    };
    expect(redirectsAccount(draft, account)).toBe(true);
    expect(draftError(draft, account)).toBe("confirmRequired");
    draft.targetConfirmed = true;
    expect(updateAccountInput(draft, account)).toEqual({
      expected_revision: 3,
      dashboard_base_url: "https://other.example",
      automatic: false,
    });
    const movedNetwork = {
      ...accountDraft(account),
      networkMode: "system" as const,
    };
    expect(updateAccountInput(movedNetwork, account)).toEqual({
      expected_revision: 3,
      network: { mode: "system" },
      automatic: false,
    });
    const zone = { ...accountDraft(account), timeZone: "UTC" };
    expect(draftError(zone, account)).toBe("confirmRequired");
    expect(updateAccountInput(zone, account)).toEqual({
      expected_revision: 3,
      time_zone: "UTC",
    });
  });
});
