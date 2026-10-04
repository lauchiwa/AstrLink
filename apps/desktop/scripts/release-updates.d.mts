export type UpdateTarget =
  | "darwin-aarch64"
  | "darwin-x86_64"
  | "windows-x86_64"
  | "linux-x86_64";

export const targets: readonly [
  "darwin-aarch64",
  "darwin-x86_64",
  "windows-x86_64",
  "linux-x86_64",
];

export interface ReleaseManifest {
  version: string;
  notes: string;
  pub_date: string;
  platforms: Record<UpdateTarget, { signature: string; url: string }>;
}

export function releaseRepository(env?: NodeJS.ProcessEnv): string;
export function publishingRepository(env?: NodeJS.ProcessEnv): string;
export function releaseVersion(tag: string): {
  version: string;
  prerelease: boolean;
};
export function stampVersion(root: string, tag: string): void;
export function verifyUpdateSignature(
  bytes: Uint8Array,
  signature: string,
  publicKey: string,
): void;
export function signingEnvironment(env?: NodeJS.ProcessEnv): void;
export function stageUpdate(
  root: string,
  target: UpdateTarget,
  tag: string,
  env?: NodeJS.ProcessEnv,
): void;
export function collectRelease(
  input: string,
  output: string,
  tag: string,
  publicKey: string,
  notes?: string,
  now?: Date,
  repository?: string,
): ReleaseManifest;
export function publishRelease(
  input: string,
  output: string,
  tag: string,
  env?: NodeJS.ProcessEnv,
): void;
