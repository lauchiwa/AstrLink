import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

// The shared migration guard scans only top-level sources. The same rules
// apply to this feature, checked here so the shared test stays untouched.
const featureDirectory = fileURLToPath(new URL("./", import.meta.url));
const srcDirectory = fileURLToPath(new URL("../../", import.meta.url));

function sources(): Array<[string, string]> {
  return readdirSync(featureDirectory)
    .filter(
      (name) =>
        (name.endsWith(".ts") || name.endsWith(".tsx")) &&
        !name.includes(".test."),
    )
    .map((name) => [name, readFileSync(`${featureDirectory}/${name}`, "utf8")]);
}

describe("check-in feature guard", () => {
  it("follows the desktop UI rules", () => {
    for (const [name, source] of sources()) {
      expect(source, name).not.toMatch(
        /(?:\bwindow\.)?\b(?:confirm|alert|prompt)\s*\(/,
      );
      expect(source, name).not.toContain("dark:");
      expect(source, name).not.toMatch(/gradient/);
      expect(source, name).not.toMatch(/\b(?:text|rounded|shadow)-\[/);
      expect(source, name).not.toMatch(/location\.reload|__astrlink_build/);
      if (name.endsWith(".tsx")) {
        expect(source, name).not.toMatch(
          /<(?:button|input|textarea|select|option|label|table|tr|td|th)\b/,
        );
      }
    }
  });

  it("declares no credential-shaped field anywhere in the feature", () => {
    for (const [name, source] of sources()) {
      expect(source, name).not.toMatch(
        /\b(?:cookies?|password|credential|access_token|refresh_token|session_token|secret)\??\s*:/i,
      );
    }
  });

  it("talks to the desktop only through its own closed command", () => {
    for (const [name, source] of sources()) {
      expect(source, name).not.toMatch(/from\s+["'](?:\.\.\/)+bridge["']/);
      expect(source, name).not.toMatch(/from\s+["']@\/bridge["']/);
      expect(source, name).not.toMatch(
        /\b(?:update_service|updateService|deleteService|saveService)\b/,
      );
      const commands = [
        ...source.matchAll(/invokeCommand(?:<[^>]*>)?\(\s*([^,)]+)/g),
      ];
      for (const [, command] of commands) {
        expect(command.trim(), name).toBe("COMMAND");
      }
    }
    expect(readFileSync(`${featureDirectory}/bridge.ts`, "utf8")).toContain(
      'const COMMAND = "fork_checkin_operation";',
    );
  });

  it("keeps the shell's registration to the lazy entry", () => {
    const app = readFileSync(`${srcDirectory}/App.tsx`, "utf8");
    const imports = [
      ...app.matchAll(/from\s+["']\.\/features\/fork-checkin\/([^"']+)["']/g),
    ].map((match) => match[1]);
    expect(imports).toEqual(["entry"]);

    const entry = readFileSync(`${featureDirectory}/entry.tsx`, "utf8");
    expect(entry).not.toMatch(/from\s+["']\.\/(?:Workspace|bridge)["']/);
    expect(entry).toContain('import("./Workspace")');
  });
});
