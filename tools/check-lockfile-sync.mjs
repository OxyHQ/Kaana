#!/usr/bin/env node
/**
 * Fail when `bun.lock` does not record what each workspace's package.json says.
 *
 * `bun install --frozen-lockfile` exits 0 when a workspace's own `version` was
 * bumped without regenerating the lockfile, and `bun publish` then rewrites a
 * `workspace:*` dependency from the STALE lockfile. That is how
 * @kaana.ai/sdk 1.0.0 reached npm depending on @kaana.ai/contracts 4.2.0, a
 * version that was never published (2026-10-15). So this compares, without an
 * install, the `name` and `version` the lockfile records for every workspace
 * path against the manifest at that path, and the workspace path set itself.
 *
 * Fix: run `bun install` and commit `bun.lock` in the SAME commit as the
 * package.json change.
 */
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dirname, '..');
const lock = readFileSync(join(root, 'bun.lock'), 'utf8');

// bun.lock is JSON with trailing commas; only the workspaces block is read.
const workspacesBlock = lock.match(/"workspaces":\s*\{([\s\S]*?)\n {2}\},/);
if (!workspacesBlock) {
  console.error('bun.lock has no workspaces block this check can read.');
  process.exit(1);
}
const recorded = new Map();
for (const match of workspacesBlock[1].matchAll(/\n {4}"([^"]*)":\s*\{([\s\S]*?)\n {4}\}/g)) {
  const [, path, body] = match;
  recorded.set(path, {
    name: body.match(/\n {6}"name":\s*"([^"]+)"/)?.[1],
    version: body.match(/\n {6}"version":\s*"([^"]+)"/)?.[1],
  });
}

const manifestPaths = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')).workspaces ?? [];
const problems = [];
const expected = new Set(['']);
for (const pattern of manifestPaths) {
  if (!pattern.includes('*')) {
    expected.add(pattern);
    continue;
  }
  // `dir/*`: every direct child of dir that is a package.
  const match = pattern.match(/^([^*]+)\/\*$/);
  if (!match) {
    problems.push(`workspace pattern "${pattern}" is not "dir/*"; this check cannot expand it.`);
    continue;
  }
  for (const child of readdirSync(join(root, match[1]), { withFileTypes: true })) {
    if (child.isDirectory() && existsSync(join(root, match[1], child.name, 'package.json')))
      expected.add(`${match[1]}/${child.name}`);
  }
}
for (const path of expected) {
  const file = join(root, path, 'package.json');
  if (!existsSync(file)) {
    problems.push(`workspace "${path}" has no package.json.`);
    continue;
  }
  const manifest = JSON.parse(readFileSync(file, 'utf8'));
  const entry = recorded.get(path);
  const label = path === '' ? 'the root' : path;
  if (!entry) {
    problems.push(`${label} is a workspace but bun.lock does not record it.`);
    continue;
  }
  if (entry.name !== manifest.name)
    problems.push(`${label} is named "${manifest.name}" but bun.lock records "${entry.name}".`);
  if (path !== '' && entry.version !== manifest.version)
    problems.push(
      `${label} is at version "${manifest.version}" but bun.lock records "${entry.version}".`,
    );
}
for (const path of recorded.keys()) {
  if (!expected.has(path))
    problems.push(`bun.lock records workspace "${path}", which package.json does not list.`);
}

if (problems.length > 0) {
  console.error(
    `bun.lock does not match the package.json files it describes.\n\n  - ${problems.join('\n  - ')}\n`,
  );
  console.error(
    'Fix: run `bun install` and commit bun.lock in the SAME commit as the package.json change.',
  );
  process.exit(1);
}
console.log(`bun.lock is in sync with every workspace (${expected.size}).`);
