import { useSyncExternalStore } from "react";

import { i18n } from "@/i18n";
import en from "./locales/en.json";
import zhCN from "./locales/zh-CN.json";

/**
 * The check-in feature keeps its strings in its own namespace, so the shared
 * catalogs stay byte-identical to upstream and a later sync cannot conflict on
 * them. Registering is idempotent; the shared instance picks the namespace up
 * whichever locale is active.
 */
export const CHECKIN_NAMESPACE = "forkCheckin";

export const checkinCatalogs = { en, "zh-CN": zhCN } as const;

for (const [locale, catalog] of Object.entries(checkinCatalogs)) {
  if (!i18n.hasResourceBundle(locale, CHECKIN_NAMESPACE)) {
    i18n.addResourceBundle(locale, CHECKIN_NAMESPACE, catalog, true, false);
  }
}

const translate = i18n.getFixedT(null, CHECKIN_NAMESPACE);

/** Translate outside React (labels computed for the shared navigation). */
export function checkinT(
  key: string,
  options?: Record<string, unknown>,
): string {
  return translate(key, options);
}

/** Codes from Core are open-ended; only catalogued ones get their own copy. */
export function checkinHasKey(key: string): boolean {
  return i18n.exists(key, { ns: CHECKIN_NAMESPACE });
}

function subscribeLanguage(onStoreChange: () => void): () => void {
  i18n.on("languageChanged", onStoreChange);
  return () => {
    i18n.off("languageChanged", onStoreChange);
  };
}

function languageSnapshot(): string {
  return i18n.language;
}

/** The feature's translator; re-renders when the shared locale changes. */
export function useCheckinT(): typeof checkinT {
  useSyncExternalStore(subscribeLanguage, languageSnapshot, languageSnapshot);
  return checkinT;
}
