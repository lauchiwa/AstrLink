import { validProxyURL } from "../../service-proxy-model";
import type { AccountDraftInput, AccountUpdateInput } from "./bridge";
import type { CheckinAccount, CheckinNetwork, NetworkMode } from "./model";

export const MAX_BOUND_SERVICES = 32;

export interface AccountDraft {
  dashboardURL: string;
  timeZone: string;
  networkMode: NetworkMode;
  proxyURL: string;
  boundServices: string[];
  automatic: boolean;
  targetConfirmed: boolean;
}

export function accountDraft(account: CheckinAccount | null): AccountDraft {
  return {
    dashboardURL: account?.dashboard_base_url ?? "",
    timeZone:
      account?.time_zone ?? Intl.DateTimeFormat().resolvedOptions().timeZone,
    networkMode: account?.network.mode ?? "direct",
    proxyURL: account?.network.proxy_url ?? "",
    boundServices: [...(account?.bound_services ?? [])],
    automatic: account?.automatic ?? false,
    targetConfirmed: false,
  };
}

/** Refuse repaired URLs before WHATWG parsing can hide an unsafe input. */
export function dashboardError(value: string): string | null {
  if (!value) return "urlRequired";
  if (value.length > 2048 || /[\s\\]/.test(value)) return "urlInvalid";
  const parts = /^(https?):\/\/([^/?#]+)([^?#]*)(.*)$/.exec(value);
  if (!parts) return "urlInvalid";
  if (parts[2].includes("@")) return "urlCredentials";
  if (parts[4]) return "urlQuery";
  try {
    const url = new URL(value);
    const path = decodeURIComponent(parts[3]);
    if (
      !url.hostname ||
      url.port === "0" ||
      parts[2].endsWith(":") ||
      /[%\\]/.test(path) ||
      [...path].some(
        (character) =>
          character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127,
      ) ||
      path.includes("//") ||
      path.split("/").some((segment) => segment === "." || segment === "..") ||
      /%2f|%5c|%25/i.test(parts[3])
    )
      return "urlInvalid";
    const host = parts[2].toLowerCase().replace(/:\d+$/, "");
    const ipv4 = host.split(".");
    const loopback =
      host === "localhost" ||
      url.hostname === "[::1]" ||
      (ipv4.length === 4 &&
        ipv4[0] === "127" &&
        ipv4.every(
          (part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255,
        ));
    if (url.protocol === "http:" && !loopback) return "urlPlainHTTP";
    return null;
  } catch {
    return "urlInvalid";
  }
}

export function timeZoneError(value: string): string | null {
  if (!value) return "timeZoneRequired";
  if (value === "Local") return "timeZoneLocal";
  if (value.length > 64 || !/^[A-Za-z_+-]+(?:\/[A-Za-z0-9_+-]+)*$/.test(value))
    return "timeZoneUnknown";
  try {
    new Intl.DateTimeFormat("en", { timeZone: value }).format(0);
    return null;
  } catch {
    return "timeZoneUnknown";
  }
}

export function draftNetwork(draft: AccountDraft): CheckinNetwork {
  return draft.networkMode === "custom"
    ? { mode: "custom", proxy_url: draft.proxyURL }
    : { mode: draft.networkMode };
}

export function redirectsAccount(
  draft: AccountDraft,
  base: CheckinAccount | null,
): boolean {
  return (
    !!base &&
    (draft.dashboardURL !== base.dashboard_base_url ||
      draft.networkMode !== base.network.mode ||
      (draft.networkMode === "custom" &&
        draft.proxyURL !== base.network.proxy_url))
  );
}

export function needsTargetConfirmation(
  draft: AccountDraft,
  base: CheckinAccount | null,
): boolean {
  return (
    !base || redirectsAccount(draft, base) || draft.timeZone !== base.time_zone
  );
}

export function draftError(
  draft: AccountDraft,
  base: CheckinAccount | null,
): string | null {
  const address = dashboardError(draft.dashboardURL);
  if (address) return address;
  const zone = timeZoneError(draft.timeZone);
  if (zone) return zone;
  if (draft.networkMode === "custom") {
    if (!draft.proxyURL) return "proxyRequired";
    if (!validProxyURL(draft.proxyURL)) return "proxyInvalid";
  }
  if (draft.boundServices.length > MAX_BOUND_SERVICES) return "servicesLimit";
  if (needsTargetConfirmation(draft, base) && !draft.targetConfirmed)
    return "confirmRequired";
  return null;
}

export function createAccountInput(draft: AccountDraft): AccountDraftInput {
  // Create is one atomic draft write, not a create followed by a binding patch.
  return {
    dashboard_base_url: draft.dashboardURL,
    time_zone: draft.timeZone,
    network: draftNetwork(draft),
  };
}

export function updateAccountInput(
  draft: AccountDraft,
  base: CheckinAccount,
): AccountUpdateInput | null {
  const patch: AccountUpdateInput = { expected_revision: base.revision };
  if (draft.dashboardURL !== base.dashboard_base_url)
    patch.dashboard_base_url = draft.dashboardURL;
  if (draft.timeZone !== base.time_zone) patch.time_zone = draft.timeZone;
  if (
    draft.networkMode !== base.network.mode ||
    (draft.networkMode === "custom" &&
      draft.proxyURL !== base.network.proxy_url)
  )
    patch.network = draftNetwork(draft);
  const bindings = [...new Set(draft.boundServices)].sort();
  if (
    JSON.stringify(bindings) !== JSON.stringify([...base.bound_services].sort())
  )
    patch.bound_services = bindings;
  const automatic =
    base.state === "connected" &&
    !redirectsAccount(draft, base) &&
    draft.automatic;
  if (automatic !== base.automatic) patch.automatic = automatic;
  return Object.keys(patch).length === 1 ? null : patch;
}
