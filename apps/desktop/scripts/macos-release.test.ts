import { execFileSync } from "node:child_process";
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  checkAdHocSignature,
  prepareMacOSSigning,
  verifyMacOSRelease,
} from "./macos-release.mjs";

const directories: string[] = [];
const config = JSON.parse(
  readFileSync(
    new URL("../src-tauri/tauri.conf.json", import.meta.url),
    "utf8",
  ),
);
function directory() {
  const root = mkdtempSync(path.join(os.tmpdir(), "astrlink-macos-signing-"));
  directories.push(root);
  return root;
}
function fixture() {
  const bundleDirectory = directory();
  mkdirSync(path.join(bundleDirectory, "macos/AstrLink.app/Contents/MacOS"), {
    recursive: true,
  });
  mkdirSync(
    path.join(bundleDirectory, "macos/AstrLink.app/Contents/Frameworks"),
    { recursive: true },
  );
  mkdirSync(path.join(bundleDirectory, "dmg"));
  writeFileSync(
    path.join(bundleDirectory, "dmg/AstrLink.dmg"),
    "synthetic disk image",
  );
  const runCommand = vi.fn((command: string, args: string[]) =>
    command === "codesign" && args[0] === "--display"
      ? "Signature=adhoc\nTeamIdentifier=not set\n"
      : "",
  );
  return { bundleDirectory, env: { MACOS_SIGNING_MODE: "adhoc" }, runCommand };
}
afterEach(() => {
  vi.restoreAllMocks();
  for (const root of directories.splice(0))
    rmSync(root, { recursive: true, force: true });
});

describe("macOS signing policy", () => {
  it("configures explicit ad-hoc signing without Apple credentials", () => {
    const root = directory();
    mkdirSync(path.join(root, "src-tauri"));
    const githubEnv = path.join(root, "github-env"),
      githubOutput = path.join(root, "github-output");
    const result = prepareMacOSSigning(root, {
      MACOS_SIGNING_MODE: "adhoc",
      GITHUB_ENV: githubEnv,
      GITHUB_OUTPUT: githubOutput,
    });
    expect(result.mode).toBe("adhoc");
    expect(JSON.parse(readFileSync(result.file, "utf8"))).toEqual({
      bundle: { macOS: { signingIdentity: "-" } },
    });
    expect(readFileSync(githubEnv, "utf8")).toBe(
      "MACOS_SIGNING_MODE=adhoc\nAPPLE_SIGNING_IDENTITY=-\n",
    );
    expect(readFileSync(githubOutput, "utf8")).toBe("mode=adhoc\n");
  });

  it("requires Developer ID configuration and never silently falls back", () => {
    const root = directory();
    mkdirSync(path.join(root, "src-tauri"));
    for (const env of [
      { MACOS_SIGNING_MODE: "developer-id" },
      {
        MACOS_SIGNING_MODE: "developer-id",
        APPLE_SIGNING_IDENTITY: "-",
        APPLE_TEAM_ID: "TESTTEAM01",
      },
      {
        MACOS_SIGNING_MODE: "developer-id",
        APPLE_SIGNING_IDENTITY: "Developer ID Application: Test\nINJECTED=1",
        APPLE_TEAM_ID: "TESTTEAM01",
      },
    ])
      expect(() => prepareMacOSSigning(root, env)).toThrow("never falls back");
    expect(
      existsSync(path.join(root, "src-tauri/tauri.signing.conf.json")),
    ).toBe(false);
    const identity = "Developer ID Application: Test Team (TESTTEAM01)";
    const result = prepareMacOSSigning(root, {
      MACOS_SIGNING_MODE: "developer-id",
      APPLE_SIGNING_IDENTITY: identity,
      APPLE_TEAM_ID: "TESTTEAM01",
    });
    expect(result.mode).toBe("developer-id");
    expect(
      JSON.parse(readFileSync(result.file, "utf8")).bundle.macOS
        .signingIdentity,
    ).toBe(identity);
  });

  it("verifies every app, sidecar, framework and DMG without claiming Gatekeeper approval", () => {
    const options = fixture();
    expect(verifyMacOSRelease(options)).toBe("adhoc");
    const verified = options.runCommand.mock.calls.filter(
      ([command, args]) => command === "codesign" && args[0] === "--verify",
    );
    expect(verified).toHaveLength(
      2 +
        config.bundle.externalBin.length +
        config.bundle.macOS.frameworks.length,
    );
    expect(
      options.runCommand.mock.calls.some(
        ([command]) => command === "xcrun" || command === "spctl",
      ),
    ).toBe(false);
    expect(
      options.runCommand.mock.calls.some(([command]) => command === "hdiutil"),
    ).toBe(true);
    expect(
      options.runCommand.mock.calls.some(
        ([command, args]) =>
          command === "codesign" &&
          args[0] === "--force" &&
          args.at(-1)?.endsWith(".dmg"),
      ),
    ).toBe(true);
    for (const details of [
      "",
      "Signature=unknown",
      "Signature=adhoc\nAuthority=Developer ID Application: Test",
    ])
      expect(() => checkAdHocSignature(details)).toThrow("ad-hoc");
  });

  it("rejects a broken nested signature before disk-image checks", () => {
    const options = fixture();
    options.runCommand.mockImplementation((command, args) => {
      if (args.at(-1)?.endsWith("astrlink-core"))
        throw new Error("invalid nested signature");
      return command === "codesign" && args[0] === "--display"
        ? "Signature=adhoc"
        : "";
    });
    expect(() => verifyMacOSRelease(options)).toThrow(
      "invalid nested signature",
    );
    expect(
      options.runCommand.mock.calls.some(([command]) => command === "hdiutil"),
    ).toBe(false);
  });

  it("does not bypass failed Developer ID or notarization checks", () => {
    const options = fixture();
    expect(() =>
      verifyMacOSRelease({
        ...options,
        env: {
          MACOS_SIGNING_MODE: "developer-id",
          APPLE_TEAM_ID: "TESTTEAM01",
        },
      }),
    ).toThrow("Developer ID Application");
    expect(
      options.runCommand.mock.calls.some(([command]) => command === "hdiutil"),
    ).toBe(false);
    const signature =
      "Authority=Developer ID Application: Test Team (TESTTEAM01)\nTeamIdentifier=TESTTEAM01\nTimestamp=test\nCodeDirectory v=20500 flags=0x10000(runtime)\n";
    options.runCommand.mockImplementation((command, args) => {
      if (command === "codesign" && args[0] === "--display") return signature;
      if (command === "xcrun" && args[0] === "notarytool") {
        if (args[1] === "submit")
          return JSON.stringify({ id: "01234567-89ab-cdef-0123-456789abcdef" });
        if (args[1] === "wait") return JSON.stringify({ status: "Rejected" });
        if (args[1] === "log") return JSON.stringify({ issues: [] });
      }
      return "";
    });
    expect(() =>
      verifyMacOSRelease({
        ...options,
        env: {
          MACOS_SIGNING_MODE: "developer-id",
          APPLE_TEAM_ID: "TESTTEAM01",
        },
      }),
    ).toThrow("Rejected");
    expect(
      options.runCommand.mock.calls.some(
        ([command, args]) => command === "spctl" || args[0] === "stapler",
      ),
    ).toBe(false);
  });

  it.skipIf(process.platform !== "darwin")(
    "verifies a real ad-hoc signed app, nested Mach-O files and DMG",
    () => {
      const options = fixture();
      const app = path.join(options.bundleDirectory, "macos/AstrLink.app");
      writeFileSync(
        path.join(app, "Contents/Info.plist"),
        '<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>astrlink-desktop</string><key>CFBundleIdentifier</key><string>com.astrlink.signing-fixture</string></dict></plist>',
      );
      for (const binary of [
        "astrlink-desktop",
        ...config.bundle.externalBin.map((file: string) => path.basename(file)),
      ]) {
        const file = path.join(app, "Contents/MacOS", binary);
        cpSync("/usr/bin/true", file);
        execFileSync("codesign", ["--force", "--sign", "-", file], {
          stdio: "pipe",
        });
      }
      const source = path.join(options.bundleDirectory, "fixture.c");
      writeFileSync(source, "int signing_fixture(void) { return 1; }\n");
      for (const framework of config.bundle.macOS.frameworks) {
        const file = path.join(
          app,
          "Contents/Frameworks",
          path.basename(framework),
        );
        execFileSync("cc", ["-dynamiclib", source, "-o", file], {
          stdio: "pipe",
        });
        execFileSync("codesign", ["--force", "--sign", "-", file], {
          stdio: "pipe",
        });
      }
      execFileSync("codesign", ["--force", "--sign", "-", app], {
        stdio: "pipe",
      });
      const dmg = path.join(options.bundleDirectory, "dmg/AstrLink.dmg");
      rmSync(dmg);
      execFileSync(
        "hdiutil",
        [
          "create",
          "-quiet",
          "-format",
          "UDZO",
          "-srcfolder",
          path.dirname(app),
          dmg,
        ],
        { stdio: "pipe" },
      );
      // Match Tauri's unsigned DMG output: finalization must sign it explicitly.
      expect(
        verifyMacOSRelease({
          bundleDirectory: options.bundleDirectory,
          env: options.env,
        }),
      ).toBe("adhoc");
      writeFileSync(
        path.join(
          app,
          "Contents/Frameworks",
          path.basename(config.bundle.macOS.frameworks[0]),
        ),
        "tampered",
      );
      expect(() =>
        verifyMacOSRelease({
          bundleDirectory: options.bundleDirectory,
          env: options.env,
        }),
      ).toThrow("codesign failed");
    },
    90_000,
  );
});
