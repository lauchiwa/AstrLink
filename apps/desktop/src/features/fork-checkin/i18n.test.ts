import { describe, expect, it } from "vitest";

import { i18n } from "@/i18n";
import sharedEn from "@/i18n/locales/en.json";
import { CHECKIN_NAMESPACE, checkinCatalogs, checkinT } from "./i18n";
import en from "./locales/en.json";
import zhCN from "./locales/zh-CN.json";

function flatten(value: unknown, prefix = ""): Map<string, string> {
  if (typeof value === "string") return new Map([[prefix, value]]);
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`catalog value at ${prefix || "$"} must be an object`);
  }
  const entries = new Map<string, string>();
  for (const [key, nested] of Object.entries(value)) {
    for (const entry of flatten(nested, prefix ? `${prefix}.${key}` : key)) {
      entries.set(...entry);
    }
  }
  return entries;
}

function placeholders(text: string): string[] {
  return [...text.matchAll(/\{\{\s*(\w+)\s*\}\}/g)]
    .map((match) => match[1])
    .sort();
}

describe("check-in catalogs", () => {
  it("keeps the same keys and placeholders in en and zh-CN", () => {
    const english = flatten(en);
    const chinese = flatten(zhCN);

    expect([...chinese.keys()].sort()).toEqual([...english.keys()].sort());
    for (const [key, text] of english) {
      expect(placeholders(chinese.get(key) ?? ""), key).toEqual(
        placeholders(text),
      );
      // `count` would switch i18next to plural keys this catalog lacks.
      expect(placeholders(text), key).not.toContain("count");
    }
  });

  it("lives in its own namespace and leaves the shared catalog alone", () => {
    expect(CHECKIN_NAMESPACE).not.toBe("translation");
    expect(i18n.getResourceBundle("en", "translation")).toEqual(sharedEn);
    for (const locale of Object.keys(checkinCatalogs)) {
      expect(i18n.hasResourceBundle(locale, CHECKIN_NAMESPACE)).toBe(true);
    }
    expect(i18n.exists("workspace.title")).toBe(false);
  });

  it("follows the shared locale", async () => {
    expect(checkinT("workspace.title")).toBe("中转站签到");

    await i18n.changeLanguage("en");
    try {
      expect(checkinT("workspace.title")).toBe("Relay check-in");
    } finally {
      await i18n.changeLanguage("zh-CN");
    }
  });
});
