import { afterEach, describe, expect, it, vi } from "vitest";
import releaseConfiguration from "../release-repository.json";
import {
  browserUpdateSnapshot,
  parseUpdatePreferences,
  parseUpdateSnapshot,
  updateBusy,
  updateNotice,
  updateRetryWait,
} from "./update-model";

describe("update IPC", () => {
  afterEach(() => vi.unstubAllEnvs());

  it("keeps the browser bootstrap on the same configured repository as packaging", () => {
    vi.stubEnv("ASTRLINK_RELEASE_REPOSITORY", undefined);
    vi.stubEnv("GITHUB_REPOSITORY", undefined);
    expect(browserUpdateSnapshot().repository).toBe(
      releaseConfiguration.repository,
    );
    vi.stubEnv("GITHUB_REPOSITORY", "actions/desktop");
    expect(browserUpdateSnapshot().repository).toBe("actions/desktop");
    vi.stubEnv("ASTRLINK_RELEASE_REPOSITORY", " fork/desktop ");
    expect(browserUpdateSnapshot().repository).toBe("fork/desktop");
  });

  it("accepts browser snapshots and optional download lengths", () => {
    const value = browserUpdateSnapshot();
    expect(parseUpdateSnapshot(value)).toEqual(value);
    expect(updateBusy({ ...value, phase: "downloading" })).toBe(true);
    expect(updateBusy({ ...value, phase: "ready" })).toBe(false);
  });
  it("rejects malformed states, preferences and external release URLs", () => {
    for (const patch of [
      { phase: "done" },
      { downloaded_bytes: -1 },
      { total_bytes: "100" },
      { revision: NaN },
      { configured: 1 },
      { error_code: undefined },
      { latest_version: 123 },
      { repository: undefined },
      { repository: "../AstrLink" },
      { repository: "fork/.." },
      { repository: "fork/desktop/extra" },
      { retry_at: undefined },
      { retry_at: 60 },
    ]) {
      expect(() =>
        parseUpdateSnapshot({ ...browserUpdateSnapshot(), ...patch }),
      ).toThrow();
    }
    expect(() =>
      parseUpdatePreferences({
        auto_check: true,
        auto_download: true,
        channel: "nightly",
      }),
    ).toThrow();
    expect(() =>
      parseUpdateSnapshot({
        ...browserUpdateSnapshot(),
        release: {
          version: "2.0.0",
          notes: "",
          published_at: null,
          url: "https://example.com/installer",
        },
      }),
    ).toThrow();
  });
  it("accepts release links only from the host's configured repository and version", () => {
    for (const repository of ["lauchiwa/AstrLink", "Calcium-Ion/AstrLink"]) {
      const value = {
        ...browserUpdateSnapshot(),
        repository,
        release: {
          version: "1.0.0",
          notes: "",
          published_at: null,
          url: `https://github.com/${repository}/releases/tag/v1.0.0`,
        },
      };
      expect(parseUpdateSnapshot(value)).toEqual(value);
      expect(
        parseUpdateSnapshot({
          ...value,
          release: {
            ...value.release,
            url: `https://github.com/${repository}/releases/tag/1.0.0`,
          },
        }).release?.version,
      ).toBe("1.0.0");
      expect(
        parseUpdateSnapshot({
          ...value,
          release: {
            ...value.release,
            version: "1.0.0+build.1",
            url: `https://github.com/${repository}/releases/tag/v1.0.0%2Bbuild.1`,
          },
        }).release?.version,
      ).toBe("1.0.0+build.1");
      const other =
        repository === "lauchiwa/AstrLink"
          ? "Calcium-Ion/AstrLink"
          : "lauchiwa/AstrLink";
      for (const url of [
        value.release.url.replace(repository, other),
        value.release.url.replace("https:", "http:"),
        value.release.url.replace("github.com", "github.com.evil"),
        value.release.url.replace("v1.0.0", "v2.0.0"),
        value.release.url + "/extra",
        value.release.url + "?download=1",
        value.release.url + "#fragment",
      ]) {
        expect(() =>
          parseUpdateSnapshot({ ...value, release: { ...value.release, url } }),
        ).toThrow();
      }
      const credentialed = new URL(value.release.url);
      credentialed.username = "test-user";
      expect(() =>
        parseUpdateSnapshot({
          ...value,
          release: { ...value.release, url: credentialed.toString() },
        }),
      ).toThrow();
    }
  });

  it("keeps the checked latest version even when no installation is needed", () => {
    const value = {
      ...browserUpdateSnapshot(),
      phase: "up_to_date" as const,
      current_version: "1.2.0",
      latest_version: "1.1.0",
    };
    expect(parseUpdateSnapshot(value).latest_version).toBe("1.1.0");
    expect(
      parseUpdateSnapshot({ ...value, latest_version: undefined })
        .latest_version,
    ).toBeNull();
  });
  it("only announces update states that wait on the operator", () => {
    const release = {
      version: "1.1.0",
      notes: "",
      published_at: null,
      url: `https://github.com/${browserUpdateSnapshot().repository}/releases/tag/v1.1.0`,
    };
    const base = { ...browserUpdateSnapshot(), release };
    const manualDownload = {
      ...base.preferences,
      auto_download: false,
    };
    expect(updateNotice({ ...base, phase: "available" })).toBeNull();
    expect(
      updateNotice({
        ...base,
        phase: "available",
        preferences: manualDownload,
      }),
    ).toBe("available");
    expect(updateNotice({ ...base, phase: "manual" })).toBe("manual");
    expect(updateNotice({ ...base, phase: "ready" })).toBe("ready");
    expect(updateNotice({ ...base, phase: "downloading" })).toBeNull();
    expect(updateNotice({ ...base, phase: "ready", release: null })).toBeNull();
  });
  it("counts whole seconds until the host accepts another check", () => {
    const now = Date.parse("2026-10-07T04:00:00Z");
    const at = (retry_at: string | null) =>
      updateRetryWait({ ...browserUpdateSnapshot(), retry_at }, now);
    expect(at(null)).toBe(0);
    expect(at("invalid")).toBe(0);
    expect(at("2026-10-07T03:59:59Z")).toBe(0);
    expect(at("2026-10-07T04:00:00.200Z")).toBe(1);
    expect(at("2026-10-07T04:12:17Z")).toBe(737);
  });
});
