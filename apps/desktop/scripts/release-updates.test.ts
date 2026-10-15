import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { createHash, generateKeyPairSync, sign } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
  collectRelease,
  attachRelease,
  extendRelease,
  macOSSigningMode,
  newestStable,
  publishingRepository,
  releaseRepository,
  releaseTargets,
  releaseVersion,
  signingEnvironment,
  stampVersion,
  stageUpdate,
  targets,
  verifyUpdateSignature,
} from "./release-updates.mjs";

const sha256 = (bytes: Buffer) =>
  createHash("sha256").update(bytes).digest("hex");

function signingFixture(bytes: Buffer) {
  const { publicKey, privateKey } = generateKeyPairSync("ed25519");
  const id = Buffer.from("0102030405060708", "hex");
  const key = Buffer.concat([
    Buffer.from("Ed"),
    id,
    publicKey.export({ type: "spki", format: "der" }).subarray(-32),
  ]);
  const signature = sign(
    null,
    createHash("blake2b512").update(bytes).digest(),
    privateKey,
  );
  const comment = "timestamp:1\tfile:test";
  const global = sign(
    null,
    Buffer.concat([signature, Buffer.from(comment)]),
    privateKey,
  );
  return {
    publicKey: Buffer.from(
      `untrusted comment: test key\n${key.toString("base64")}\n`,
    ).toString("base64"),
    signature: Buffer.from(
      `untrusted comment: test signature\n${Buffer.concat([Buffer.from("ED"), id, signature]).toString("base64")}\ntrusted comment: ${comment}\n${global.toString("base64")}\n`,
    ).toString("base64"),
  };
}
function stagePlatform(
  input: string,
  target: string,
  bytes: Buffer,
  signature: string,
) {
  const dir = path.join(input, target);
  mkdirSync(dir);
  const filename = `${target}.exe`;
  writeFileSync(path.join(dir, filename), bytes);
  writeFileSync(path.join(dir, `${filename}.sig`), signature);
  writeFileSync(
    path.join(dir, `${target}.json`),
    JSON.stringify({
      target,
      repository: releaseRepository(),
      ...(target.startsWith("darwin-")
        ? { macos_signing_mode: macOSSigningMode() }
        : {}),
      tag: "v1.0.0",
      version: "1.0.0",
      file: filename,
      signature,
      sha256: sha256(bytes),
    }),
  );
}
const temporary: string[] = [];
const directory = () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "astrlink-update-test-"));
  temporary.push(dir);
  return dir;
};
afterEach(() => {
  for (const dir of temporary.splice(0))
    rmSync(dir, { recursive: true, force: true });
});

describe("release updates", () => {
  it("resolves the explicit repository before Actions and local configuration", () => {
    const configured = JSON.parse(
      readFileSync(
        new URL("../release-repository.json", import.meta.url),
        "utf8",
      ),
    ).repository;
    expect(releaseRepository({})).toBe(configured);
    expect(releaseRepository({ GITHUB_REPOSITORY: "actions/desktop" })).toBe(
      "actions/desktop",
    );
    expect(
      releaseRepository({
        GITHUB_REPOSITORY: "actions/desktop",
        ASTRLINK_RELEASE_REPOSITORY: " fork/desktop ",
      }),
    ).toBe("fork/desktop");
    for (const slug of [
      "",
      " ",
      "https://github.com/fork/desktop",
      "fork/desktop/extra",
      "../desktop",
      "fork/..",
      "fork/.",
      "-fork/desktop",
      "fork-/desktop",
      "fork_name/desktop",
      "fork/desktop?tag=v1",
      "fork/desktop#fragment",
      "fork/desk%2Ftop",
      "fork/desk\ntop",
      "fork/桌面",
      `${"a".repeat(40)}/desktop`,
      `fork/${"a".repeat(101)}`,
    ]) {
      expect(() =>
        releaseRepository({
          GITHUB_REPOSITORY: "upstream/desktop",
          ASTRLINK_RELEASE_REPOSITORY: slug,
        }),
      ).toThrow("owner/repository");
    }
    for (const slug of [
      "lauchiwa/AstrLink",
      "Calcium-Ion/AstrLink",
      "a/.github",
      "a/repo_name-1.2",
    ])
      expect(releaseRepository({ ASTRLINK_RELEASE_REPOSITORY: slug })).toBe(
        slug,
      );
  });

  it("uses the explicit macOS signing policy without credential-based fallback", () => {
    expect(macOSSigningMode({})).toBe("adhoc");
    expect(macOSSigningMode({ MACOS_SIGNING_MODE: "" })).toBe("adhoc");
    expect(macOSSigningMode({ MACOS_SIGNING_MODE: "developer-id" })).toBe(
      "developer-id",
    );
    expect(macOSSigningMode({ MACOS_SIGNING_MODE: " adhoc " })).toBe("adhoc");
    for (const mode of ["auto", "unsigned", "developer", "adhoc\nINJECTED=1"])
      expect(() => macOSSigningMode({ MACOS_SIGNING_MODE: mode })).toThrow(
        "MACOS_SIGNING_MODE",
      );
    expect(() =>
      signingEnvironment({ MACOS_SIGNING_MODE: "unsigned" }),
    ).toThrow("MACOS_SIGNING_MODE");
  });

  it("allows publishing only to the current Actions repository", () => {
    for (const repository of ["lauchiwa/AstrLink", "Calcium-Ion/AstrLink"]) {
      expect(publishingRepository({ GITHUB_REPOSITORY: repository })).toBe(
        repository,
      );
      expect(
        publishingRepository({
          GITHUB_REPOSITORY: repository,
          ASTRLINK_RELEASE_REPOSITORY: repository,
        }),
      ).toBe(repository);
    }
    expect(() => publishingRepository({})).toThrow(
      "current Actions repository",
    );
    expect(() =>
      publishingRepository({
        GITHUB_REPOSITORY: "lauchiwa/AstrLink",
        ASTRLINK_RELEASE_REPOSITORY: "Calcium-Ion/AstrLink",
      }),
    ).toThrow("current Actions repository");
  });

  it("binds every packaging workflow and publication to its own repository", () => {
    for (const file of [
      "release",
      "release-macos-x86_64",
      "macos-package",
      "windows-package",
      "linux-package",
    ]) {
      const workflow = readFileSync(
        new URL(`../../../.github/workflows/${file}.yml`, import.meta.url),
        "utf8",
      );
      expect(workflow).toContain(
        "ASTRLINK_RELEASE_REPOSITORY: ${{ github.repository }}",
      );
      expect(workflow).toContain(
        "MACOS_SIGNING_MODE: ${{ vars.MACOS_SIGNING_MODE }}",
      );
      expect(workflow).not.toContain(
        "if: github.repository == 'Calcium-Ion/AstrLink'",
      );
    }
  });

  it("does not force Finder layout on headless macOS runners", () => {
    const workflow = readFileSync(
      new URL("../../../.github/workflows/macos-package.yml", import.meta.url),
      "utf8",
    );
    expect(workflow).not.toContain('TAURI_BUNDLER_DMG_IGNORE_CI: "true"');
    // Bundling is split from the build so a retry does not rebuild. Both halves
    // must still carry the generated signing policy.
    expect(workflow).toContain(
      "tauri build --ci --no-bundle --config src-tauri/tauri.signing.conf.json",
    );
    expect(workflow).toContain(
      "tauri bundle --ci --verbose --bundles app,dmg --config src-tauri/tauri.signing.conf.json",
    );
    // Keep upstream's notarization step rather than replacing it, so the next
    // sync does not re-conflict on that hunk. The ad-hoc policy adapts by
    // repointing macos:notarize at the fork's wrapper, which resolves the
    // signing mode in code and fails closed on an unknown value.
    expect(workflow).toContain("run: bun run macos:notarize");
    const manifest = JSON.parse(
      readFileSync(new URL("../package.json", import.meta.url), "utf8"),
    );
    expect(manifest.scripts["macos:notarize"]).toBe(
      "bun scripts/macos-release.mjs verify",
    );
  });

  it("retries an existing release tag without building a moving branch", () => {
    const workflow = (name: string) =>
      readFileSync(
        new URL(`../../../.github/workflows/${name}.yml`, import.meta.url),
        "utf8",
      );
    const release = workflow("release");
    expect(release).toContain("workflow_dispatch:");
    expect(release).toContain(
      "RELEASE_TAG: ${{ inputs.release_tag || github.ref_name }}",
    );
    expect(
      release.match(
        /ref: refs\/tags\/\$\{\{ inputs.release_tag \|\| github.ref_name \}\}/g,
      ),
    ).toHaveLength(2);
    for (const name of ["macos-package", "linux-package", "windows-package"]) {
      expect(
        workflow(name).match(
          /ref: \$\{\{ inputs.release_tag && format\('refs\/tags\/\{0\}', inputs.release_tag\) \|\| github.ref \}\}/g,
        ),
      ).toHaveLength(1);
    }
  });

  it("round-trips real Tauri CLI signatures through all four platform manifests", () => {
    const root = directory();
    mkdirSync(path.join(root, "src-tauri"));
    const cli = fileURLToPath(
      new URL("../node_modules/@tauri-apps/cli/tauri.js", import.meta.url),
    );
    const keyPath = path.join(root, "test.key");
    execFileSync(
      "bun",
      [
        cli,
        "signer",
        "generate",
        "--ci",
        "--password",
        "",
        "--write-keys",
        keyPath,
      ],
      { stdio: "pipe" },
    );
    const env = {
      ...process.env,
      TAURI_SIGNING_PRIVATE_KEY: readFileSync(keyPath, "utf8").trim(),
      TAURI_UPDATER_PUBLIC_KEY: readFileSync(`${keyPath}.pub`, "utf8").trim(),
      TAURI_SIGNING_PRIVATE_KEY_PASSWORD: "",
    };
    writeFileSync(
      path.join(root, "package.json"),
      JSON.stringify({ version: "1.0.0", scripts: { tauri: `bun "${cli}"` } }),
    );
    writeFileSync(
      path.join(root, "src-tauri/tauri.conf.json"),
      JSON.stringify({ version: "1.0.0" }),
    );
    const packages = path.join(root, "package");
    mkdirSync(packages);
    for (const arch of ["arm64", "x86_64"])
      writeFileSync(
        path.join(packages, `AstrLink-macOS-${arch}.app.tar.gz`),
        `test-only archive ${arch}`,
      );
    for (const [folder, file] of [
      ["x86_64-pc-windows-msvc/release/bundle/nsis", "test-setup.exe"],
      ["release/bundle/appimage", "test.AppImage"],
    ]) {
      const destination = path.join(root, "src-tauri/target", folder);
      mkdirSync(destination, { recursive: true });
      writeFileSync(
        path.join(destination, file),
        `test-only installer ${file}`,
      );
    }
    for (const target of targets) stageUpdate(root, target, "v1.0.0", env);
    const release = path.join(root, "release");
    const manifest = collectRelease(
      packages,
      release,
      "v1.0.0",
      env.TAURI_UPDATER_PUBLIC_KEY,
    );
    expect(Object.keys(manifest.platforms)).toEqual(releaseTargets);
    const extended = extendRelease(
      packages,
      release,
      path.join(root, "attach"),
      "v1.0.0",
      "darwin-x86_64",
      env.TAURI_UPDATER_PUBLIC_KEY,
    );
    expect(Object.keys(extended.platforms).sort()).toEqual([...targets].sort());
  });

  it("validates release tags and identifies preview channels", () => {
    expect(releaseVersion("v1.2.3")).toEqual({
      version: "1.2.3",
      prerelease: false,
    });
    expect(releaseVersion("1.2.3-rc.2+build.4").prerelease).toBe(true);
    for (const tag of [
      "latest",
      "v01.2.3",
      "1.2",
      "1.2.3-01",
      "1.2.3+",
      "1.2.3/evil",
    ])
      expect(() => releaseVersion(tag)).toThrow();
  });
  it("marks only the highest stable version as GitHub's latest release", () => {
    const published = (tag_name: string, extra = {}) => ({
      tag_name,
      draft: false,
      prerelease: false,
      ...extra,
    });
    const releases = [
      published("v0.9.0"),
      published("v0.10.0"),
      published("v1.0.0-rc.1", { prerelease: true }),
      published("v2.0.0", { draft: true }),
      published("v3.0.0", { prerelease: true }),
      published("latest"),
    ];
    expect(newestStable("v0.10.1", releases)).toBe(true);
    expect(newestStable("v1.0.0", releases)).toBe(true);
    expect(newestStable("v0.10.0", releases)).toBe(true);
    // A patch for an older line must not move stable clients backwards.
    expect(newestStable("v0.9.1", releases)).toBe(false);
    expect(newestStable("v1.1.0-beta.1", releases)).toBe(false);
    expect(newestStable("v1.0.0", [])).toBe(true);
  });

  it("stamps all desktop versions without changing dependencies", () => {
    const dir = directory();
    mkdirSync(path.join(dir, "src-tauri"));
    writeFileSync(
      path.join(dir, "package.json"),
      '{"version":"0.1.0","name":"desktop"}',
    );
    writeFileSync(
      path.join(dir, "src-tauri/tauri.conf.json"),
      '{"version":"0.1.0"}',
    );
    writeFileSync(
      path.join(dir, "src-tauri/Cargo.toml"),
      '[package]\nname = "astrlink-desktop"\nversion = "0.1.0"\n[dependencies]\nother = "1"\n',
    );
    writeFileSync(
      path.join(dir, "src-tauri/Cargo.lock"),
      '[[package]]\nname = "astrlink-desktop"\nversion = "0.1.0"\n[[package]]\nname = "other"\nversion = "1.0.0"\n',
    );
    // Git checkouts on Windows may contain CRLF.
    for (const file of ["src-tauri/Cargo.toml", "src-tauri/Cargo.lock"]) {
      const target = path.join(dir, file);
      writeFileSync(
        target,
        readFileSync(target, "utf8").replaceAll("\n", "\r\n"),
      );
    }
    stampVersion(dir, "v2.0.0-beta.1");
    expect(
      JSON.parse(readFileSync(path.join(dir, "package.json"), "utf8")).version,
    ).toBe("2.0.0-beta.1");
    expect(
      readFileSync(path.join(dir, "src-tauri/Cargo.lock"), "utf8"),
    ).toContain('name = "other"\r\nversion = "1.0.0"');
    expect(
      readFileSync(path.join(dir, "src-tauri/Cargo.toml"), "utf8"),
    ).toContain('version = "2.0.0-beta.1"');
  });
  it("verifies both artifact bytes and the trusted comment", () => {
    const bytes = Buffer.from("a signed test artifact"),
      keys = signingFixture(bytes);
    expect(() =>
      verifyUpdateSignature(bytes, keys.signature, keys.publicKey),
    ).not.toThrow();
    expect(() =>
      verifyUpdateSignature(
        Buffer.from("tampered"),
        keys.signature,
        keys.publicKey,
      ),
    ).toThrow("signature mismatch");
    const changed = Buffer.from(
      Buffer.from(keys.signature, "base64")
        .toString()
        .replace("timestamp:1", "timestamp:2"),
    ).toString("base64");
    expect(() => verifyUpdateSignature(bytes, changed, keys.publicKey)).toThrow(
      "trusted comment",
    );
    expect(() =>
      verifyUpdateSignature(
        bytes,
        keys.signature,
        signingFixture(bytes).publicKey,
      ),
    ).toThrow();
  });
  it("requires signing configuration only for update releases", () => {
    expect(() => signingEnvironment({})).toThrow("PUBLIC_KEY");
    expect(() =>
      signingEnvironment({ TAURI_UPDATER_PUBLIC_KEY: "public" }),
    ).toThrow("PRIVATE_KEY");
  });
  it("publishes a manifest only after every platform is present and verified", () => {
    const input = directory(),
      output = path.join(directory(), "release"),
      bytes = Buffer.from("signed installer");
    const keys = signingFixture(bytes);
    const platform = (target: string) =>
      stagePlatform(input, target, bytes, keys.signature);
    for (const target of releaseTargets.slice(0, -1)) platform(target);
    expect(() =>
      collectRelease(input, output, "v1.0.0", keys.publicKey),
    ).toThrow("Missing or duplicate");
    expect(existsSync(output)).toBe(false);
    platform(releaseTargets.at(-1)!);
    const manifest = collectRelease(
      input,
      output,
      "v1.0.0",
      keys.publicKey,
      "Release notes",
    );
    expect(Object.keys(manifest.platforms)).toEqual(releaseTargets);
    for (const mode of ["adhoc", "developer-id"] as const) {
      for (const target of releaseTargets.filter((target) =>
        target.startsWith("darwin-"),
      )) {
        const file = path.join(input, target, `${target}.json`);
        const fragment = JSON.parse(readFileSync(file, "utf8"));
        fragment.macos_signing_mode = mode;
        writeFileSync(file, JSON.stringify(fragment));
      }
      const collected = collectRelease(
        input,
        path.join(directory(), "release"),
        "v1.0.0",
        keys.publicKey,
        "Release notes",
        new Date(),
        releaseRepository(),
        mode,
      );
      if (mode === "adhoc") {
        expect(collected.notes).toContain("not notarized");
        expect(collected.notes).toContain("Open Anyway");
        expect(collected.notes).toContain("Release notes");
        expect(collected.notes).toContain("Do not disable Gatekeeper");
      } else expect(collected.notes).toBe("Release notes");
      const wrongOutput = path.join(directory(), "wrong-mode");
      expect(() =>
        collectRelease(
          input,
          wrongOutput,
          "v1.0.0",
          keys.publicKey,
          "",
          new Date(),
          releaseRepository(),
          mode === "adhoc" ? "developer-id" : "adhoc",
        ),
      ).toThrow("signing mode mismatch");
      expect(existsSync(wrongOutput)).toBe(false);
    }
    for (const target of releaseTargets.filter((target) =>
      target.startsWith("darwin-"),
    )) {
      const file = path.join(input, target, `${target}.json`);
      const fragment = JSON.parse(readFileSync(file, "utf8"));
      fragment.macos_signing_mode = macOSSigningMode();
      writeFileSync(file, JSON.stringify(fragment));
    }
    // Release publishes darwin-aarch64 only; Intel macOS is attached later.
    const mixed = path.join(input, "darwin-aarch64", "darwin-aarch64.json");
    const original = readFileSync(mixed, "utf8");
    const fragment = JSON.parse(original);
    delete fragment.macos_signing_mode;
    writeFileSync(mixed, JSON.stringify(fragment));
    expect(() =>
      collectRelease(
        input,
        path.join(directory(), "missing-mode"),
        "v1.0.0",
        keys.publicKey,
      ),
    ).toThrow("signing mode mismatch");
    writeFileSync(mixed, original);
    expect(manifest.platforms[releaseTargets[0]].url).toContain(
      "/releases/download/v1.0.0/",
    );
    for (const repository of ["lauchiwa/AstrLink", "Calcium-Ion/AstrLink"]) {
      for (const target of releaseTargets) {
        const file = path.join(input, target, `${target}.json`);
        const fragment = JSON.parse(readFileSync(file, "utf8"));
        fragment.repository = repository;
        writeFileSync(file, JSON.stringify(fragment));
      }
      const forkManifest = collectRelease(
        input,
        path.join(directory(), "release"),
        "v1.0.0",
        keys.publicKey,
        "",
        new Date(),
        repository,
      );
      for (const target of releaseTargets)
        expect(forkManifest.platforms[target].url).toContain(
          `https://github.com/${repository}/releases/download/v1.0.0/`,
        );
      const wrongOutput = path.join(directory(), "wrong-repository");
      expect(() =>
        collectRelease(
          input,
          wrongOutput,
          "v1.0.0",
          keys.publicKey,
          "",
          new Date(),
          "another-owner/AstrLink",
        ),
      ).toThrow("Repository mismatch");
      expect(existsSync(wrongOutput)).toBe(false);
    }
    for (const target of releaseTargets) {
      const file = path.join(input, target, `${target}.json`);
      const fragment = JSON.parse(readFileSync(file, "utf8"));
      fragment.repository = releaseRepository();
      writeFileSync(file, JSON.stringify(fragment));
    }
    expect(() =>
      collectRelease(
        input,
        path.join(directory(), "bad"),
        "v2.0.0",
        keys.publicKey,
      ),
    ).toThrow("Version");
    writeFileSync(
      path.join(input, releaseTargets[0], `${releaseTargets[0]}.exe`),
      "corrupted",
    );
    expect(() =>
      collectRelease(
        input,
        path.join(directory(), "bad"),
        "v1.0.0",
        keys.publicKey,
      ),
    ).toThrow("checksum");
  });
  it("adds a late platform to a published release without replacing a listed package", () => {
    const bytes = Buffer.from("signed installer"),
      keys = signingFixture(bytes);
    const input = directory(),
      late = directory(),
      published = path.join(directory(), "release"),
      output = path.join(directory(), "attach");
    for (const target of releaseTargets)
      stagePlatform(input, target, bytes, keys.signature);
    // Fork: collectRelease may prefix the ad-hoc macOS notice; extendRelease
    // must keep the published notes as they are.
    const release = collectRelease(
      input,
      published,
      "v1.0.0",
      keys.publicKey,
      "Notes",
    );
    stagePlatform(late, "darwin-x86_64", bytes, keys.signature);
    const extend = (from: string, to: string, tag = "v1.0.0") =>
      extendRelease(late, from, to, tag, "darwin-x86_64", keys.publicKey);

    const manifest = extend(published, output);
    expect(Object.keys(manifest.platforms)).toEqual([
      ...releaseTargets,
      "darwin-x86_64",
    ]);
    expect(manifest.notes).toBe(release.notes);
    expect(manifest.notes).toContain("Notes");
    // Only the new package and the two rewritten manifests are uploaded.
    expect(readdirSync(output).sort()).toEqual([
      "SHA256SUMS",
      "darwin-x86_64.exe",
      "darwin-x86_64.exe.sig",
      "latest.json",
    ]);
    const sums = readFileSync(path.join(output, "SHA256SUMS"), "utf8");
    expect(sums).toContain(`${sha256(bytes)}  windows-x86_64.exe\n`);
    expect(sums).toContain(`${sha256(bytes)}  darwin-x86_64.exe\n`);
    expect(sums).toContain(
      `${sha256(readFileSync(path.join(output, "latest.json")))}  latest.json\n`,
    );
    // Release's lines stay and the two package files join them.
    const lines = (dir: string) =>
      readFileSync(path.join(dir, "SHA256SUMS"), "utf8")
        .split("\n")
        .filter(Boolean);
    expect(lines(output)).toHaveLength(lines(published).length + 2);

    // Re-running with the same package succeeds, e.g. after a failed upload.
    expect(() => extend(output, path.join(directory(), "retry"))).not.toThrow();

    const refused = path.join(directory(), "refused");
    expect(() => extend(published, refused, "v2.0.0")).toThrow(
      "Published manifest version",
    );
    const listed = JSON.parse(
      readFileSync(path.join(output, "latest.json"), "utf8"),
    );
    listed.platforms["darwin-x86_64"].url += "-other";
    writeFileSync(path.join(output, "latest.json"), JSON.stringify(listed));
    expect(() => extend(output, refused)).toThrow(
      "different darwin-x86_64 package",
    );
    writeFileSync(
      path.join(published, "SHA256SUMS"),
      readFileSync(path.join(published, "SHA256SUMS"), "utf8") +
        `${"0".repeat(64)}  darwin-x86_64.exe\n`,
    );
    expect(() => extend(published, refused)).toThrow(
      "different darwin-x86_64.exe",
    );
    expect(existsSync(refused)).toBe(false);
  });
  it("attaches a late platform only from this repository and signing mode", () => {
    const bytes = Buffer.from("signed installer"),
      keys = signingFixture(bytes);
    const rewrite = (dir: string, change: Record<string, unknown>) => {
      for (const target of readdirSync(dir)) {
        const file = path.join(dir, target, `${target}.json`);
        writeFileSync(
          file,
          JSON.stringify({
            ...JSON.parse(readFileSync(file, "utf8")),
            ...change,
          }),
        );
      }
    };
    for (const repository of ["lauchiwa/AstrLink", "Calcium-Ion/AstrLink"]) {
      for (const mode of ["adhoc", "developer-id"] as const) {
        const input = directory(),
          late = directory(),
          published = path.join(directory(), "release");
        for (const target of releaseTargets)
          stagePlatform(input, target, bytes, keys.signature);
        stagePlatform(late, "darwin-x86_64", bytes, keys.signature);
        rewrite(input, { repository, macos_signing_mode: mode });
        rewrite(late, { repository, macos_signing_mode: mode });
        const release = collectRelease(
          input,
          published,
          "v1.0.0",
          keys.publicKey,
          "Notes",
          new Date(),
          repository,
          mode,
        );
        const extend = (
          output: string,
          extendRepository: string,
          extendMode: "adhoc" | "developer-id",
        ) =>
          extendRelease(
            late,
            published,
            output,
            "v1.0.0",
            "darwin-x86_64",
            keys.publicKey,
            extendRepository,
            extendMode,
          );
        const manifest = extend(
          path.join(directory(), "attach"),
          repository,
          mode,
        );
        expect(manifest.platforms["darwin-x86_64"].url).toBe(
          `https://github.com/${repository}/releases/download/v1.0.0/darwin-x86_64.exe`,
        );
        // The ad-hoc notice is added once, by Release, never again on attach.
        expect(manifest.notes).toBe(release.notes);
        expect(manifest.notes.split("not notarized")).toHaveLength(
          mode === "adhoc" ? 2 : 1,
        );
        const refused = path.join(directory(), "refused");
        expect(() => extend(refused, "another-owner/AstrLink", mode)).toThrow(
          "Repository mismatch",
        );
        expect(() =>
          extend(
            refused,
            repository,
            mode === "adhoc" ? "developer-id" : "adhoc",
          ),
        ).toThrow("signing mode mismatch");
        expect(existsSync(refused)).toBe(false);
      }
    }
  });
  it("refuses to attach outside the current Actions repository before calling GitHub", () => {
    const output = path.join(directory(), "attach");
    for (const env of [
      {},
      {
        GITHUB_REPOSITORY: "lauchiwa/AstrLink",
        ASTRLINK_RELEASE_REPOSITORY: "Calcium-Ion/AstrLink",
      },
    ])
      expect(() =>
        attachRelease(directory(), output, "v1.0.0", "darwin-x86_64", env),
      ).toThrow("current Actions repository");
    expect(existsSync(output)).toBe(false);
  });
});
