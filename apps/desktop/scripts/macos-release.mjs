import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { macOSSigningMode } from "./release-updates.mjs";
import { checkSignature, notarizeMacOS } from "./notarize-macos.mjs";

const desktop = fileURLToPath(new URL("..", import.meta.url));
const configuration = JSON.parse(
  readFileSync(path.join(desktop, "src-tauri/tauri.conf.json"), "utf8"),
);

export function prepareMacOSSigning(root = desktop, env = process.env) {
  const mode = macOSSigningMode(env);
  const identity = mode === "adhoc" ? "-" : env.APPLE_SIGNING_IDENTITY?.trim();
  if (
    mode === "developer-id" &&
    (!identity?.startsWith("Developer ID Application: ") ||
      /[\r\n]/.test(identity) ||
      !/^[A-Z0-9]{10}$/.test(env.APPLE_TEAM_ID ?? ""))
  )
    throw new Error(
      "Developer ID mode requires APPLE_SIGNING_IDENTITY and APPLE_TEAM_ID; it never falls back to ad-hoc signing.",
    );
  const file = path.join(root, "src-tauri/tauri.signing.conf.json");
  writeFileSync(
    file,
    JSON.stringify(
      { bundle: { macOS: { signingIdentity: identity } } },
      null,
      2,
    ) + "\n",
  );
  if (env.GITHUB_ENV)
    writeFileSync(
      env.GITHUB_ENV,
      `MACOS_SIGNING_MODE=${mode}\nAPPLE_SIGNING_IDENTITY=${identity}\n`,
      { flag: "a" },
    );
  if (env.GITHUB_OUTPUT)
    writeFileSync(env.GITHUB_OUTPUT, `mode=${mode}\n`, { flag: "a" });
  console.log(
    `macOS signing policy: ${mode}. ${mode === "adhoc" ? "Apple notarization is intentionally disabled; updater signatures remain required." : "Developer ID and successful Apple notarization are required."}`,
  );
  return { mode, file };
}

function run(command, args) {
  const result = spawnSync(command, args, {
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
  });
  if (result.error) throw result.error;
  if (result.status !== 0)
    throw new Error(
      `${command} failed (${result.status}):\n${result.stdout ?? ""}${result.stderr ?? ""}`,
    );
  return `${result.stdout ?? ""}${result.stderr ?? ""}`;
}

export function checkAdHocSignature(details) {
  if (
    !details.split(/\r?\n/).includes("Signature=adhoc") ||
    /^Authority=/m.test(details)
  )
    throw new Error(
      "Ad-hoc mode requires an actual ad-hoc code signature on every packaged component.",
    );
}

export function verifyMacOSApplication(
  app,
  env = process.env,
  runCommand = run,
) {
  const mode = macOSSigningMode(env);
  if (!existsSync(app)) throw new Error(`Missing app bundle: ${app}`);
  const binaries = configuration.bundle.externalBin.map((file) =>
    path.join(app, "Contents/MacOS", path.basename(file)),
  );
  const frameworks = configuration.bundle.macOS.frameworks.map((file) =>
    path.join(app, "Contents/Frameworks", path.basename(file)),
  );
  for (const file of [app, ...binaries, ...frameworks]) {
    runCommand("codesign", ["--verify", "--deep", "--strict", file]);
    const details = runCommand("codesign", ["--display", "--verbose=4", file]);
    if (mode === "adhoc") checkAdHocSignature(details);
    else
      checkSignature(
        details,
        env.APPLE_TEAM_ID,
        file === app || binaries.includes(file),
      );
  }
  return mode;
}

export function verifyMacOSRelease({
  bundleDirectory,
  env = process.env,
  runCommand = run,
}) {
  const mode = macOSSigningMode(env);
  if (mode === "developer-id") {
    notarizeMacOS({
      bundleDirectory,
      teamID: env.APPLE_TEAM_ID,
      profile: env.APPLE_NOTARY_PROFILE || "astrlink-notary",
      keychain: env.APPLE_NOTARY_KEYCHAIN,
      runCommand,
    });
    return mode;
  }
  const app = path.join(
    bundleDirectory,
    "macos",
    `${configuration.productName}.app`,
  );
  if (!existsSync(app)) throw new Error(`Missing app bundle: ${app}`);
  const images = readdirSync(path.join(bundleDirectory, "dmg")).filter((name) =>
    name.endsWith(".dmg"),
  );
  if (images.length !== 1)
    throw new Error(
      "Expected exactly one DMG; remove stale installers before building.",
    );
  const dmg = path.join(bundleDirectory, "dmg", images[0]);
  verifyMacOSApplication(app, env, runCommand);
  runCommand("hdiutil", ["verify", dmg]);
  // Tauri signs the app and nested code with '-' but leaves its DMG unsigned.
  // Finalize the image explicitly before checksums or updater signatures exist.
  runCommand("codesign", ["--force", "--sign", "-", "--timestamp=none", dmg]);
  runCommand("codesign", ["--verify", "--deep", "--strict", dmg]);
  checkAdHocSignature(
    runCommand("codesign", ["--display", "--verbose=4", dmg]),
  );
  console.warn(
    "macOS app and DMG passed ad-hoc signature and image checks. They are NOT Apple-notarized or Gatekeeper-approved; users may need to approve trusted downloads in Privacy & Security.",
  );
  return mode;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    if (process.platform !== "darwin")
      throw new Error("macOS packaging verification must run on a Mac.");
    const command = process.argv[2];
    if (command === "prepare") prepareMacOSSigning();
    else if (command === "verify") {
      const { values } = parseArgs({
        args: process.argv.slice(3),
        options: {
          "bundle-dir": {
            type: "string",
            default: path.join(desktop, "src-tauri/target/release/bundle"),
          },
        },
      });
      verifyMacOSRelease({
        bundleDirectory: path.resolve(values["bundle-dir"]),
      });
    } else if (command === "verify-app") {
      const { values } = parseArgs({
        args: process.argv.slice(3),
        options: { app: { type: "string" } },
      });
      if (!values.app) throw new Error("Expected --app path.");
      verifyMacOSApplication(path.resolve(values.app));
    } else throw new Error("Expected prepare, verify or verify-app command.");
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
