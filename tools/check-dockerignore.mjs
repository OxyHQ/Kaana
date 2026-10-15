#!/usr/bin/env node
/**
 * Fail when the Dockerfile copies a `configs/` file that `.dockerignore` keeps
 * out of the build context.
 *
 * `.dockerignore` is an allow-list by exact name, so a new configuration file
 * baked into the image has to be admitted there too. Forgetting it passes every
 * Go test and `make check`, and fails only in the deploy, at `docker build`
 * (configs/deployment-commercial.json, 2026-10-15). This compares the two files
 * directly, without Docker.
 */
import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dirname, '..');
const dockerfile = readFileSync(join(root, 'Dockerfile'), 'utf8');
const admitted = new Set(
  readFileSync(join(root, '.dockerignore'), 'utf8')
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.startsWith('!configs/'))
    .map((line) => line.slice(1)),
);
const copied = new Set(dockerfile.match(/configs\/[A-Za-z0-9._/-]+\.json/g) ?? []);
const missing = [...copied].filter((path) => !admitted.has(path)).sort();
if (missing.length > 0) {
  console.error(
    `The Dockerfile copies configuration .dockerignore does not admit:\n  - ${missing.join('\n  - ')}\n\nAdd "!<path>" to .dockerignore, by exact name.`,
  );
  process.exit(1);
}
console.log(`.dockerignore admits every configs/ file the Dockerfile copies (${copied.size}).`);
