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
 *
 * It also fails when the deploy workflow's `push.paths` omits one of them: a
 * reviewed change to a baked file is a change to the image, and without the
 * path it merges and never deploys (configs/deployment-commercial.json,
 * 2026-10-15, the second time).
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
const workflow = readFileSync(join(root, '.github/workflows/deploy-aws.yml'), 'utf8');
const pushPaths =
  /\n {2}push:\n {4}branches: \[main\]\n {4}paths:\n((?: {6}(?:- '[^']+'|#.*)\n)+)/.exec(workflow);
if (pushPaths === null) {
  console.error('.github/workflows/deploy-aws.yml has no push.paths list this check can read.');
  process.exit(1);
}
const deployed = new Set([...pushPaths[1].matchAll(/- '([^']+)'/g)].map((match) => match[1]));
const undeployed = [...copied].filter((path) => !deployed.has(path)).sort();
if (undeployed.length > 0) {
  console.error(
    `The Dockerfile bakes configuration a merge to main does not deploy:\n  - ${undeployed.join('\n  - ')}\n\nAdd "- '<path>'" to push.paths in .github/workflows/deploy-aws.yml, by exact name.`,
  );
  process.exit(1);
}
console.log(
  `.dockerignore admits, and a merge deploys, every configs/ file the Dockerfile copies (${copied.size}).`,
);
