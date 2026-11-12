#!/usr/bin/env node
/**
 * Fail when a documentation link is broken or a top-level doc is unreachable.
 *
 * Two checks, because both have gone wrong before the gate existed:
 *
 * 1. Every relative Markdown link in the repository's own docs (AGENTS.md,
 *    README.md, docs/**, packages/*\/README.md) must resolve to a file, and a
 *    `#fragment` must match a heading in that file (GitHub's slug rules).
 *    `docs/audits/` is an archive of proof bundles and is not checked.
 * 2. Every `docs/*.md` must be linked from AGENTS.md or README.md, so a new
 *    document is found by the index an agent reads first.
 *
 * A third check covers the public pages. The Oxy website publishes EVERY `.md`
 * and `.mdx` under the `docsPath` of `docs.config.json`, so that path is
 * `docs/public/` and nothing else may be published by accident; each page there
 * needs `title`, `description` and `order` frontmatter.
 *
 * Absolute URLs are not fetched; this gate needs no network.
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';

const root = resolve(import.meta.dirname, '..');

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (name === 'node_modules' || name === '.git' || name === 'audits') continue;
    if (statSync(path).isDirectory()) walk(path, out);
    else if (name.endsWith('.md')) out.push(path);
  }
  return out;
}

const files = [
  join(root, 'AGENTS.md'),
  join(root, 'README.md'),
  ...walk(join(root, 'docs')),
  ...readdirSync(join(root, 'packages'))
    .map((name) => join(root, 'packages', name, 'README.md'))
    .filter((path) => existsSync(path)),
];

function slug(heading) {
  return heading
    .trim()
    .toLowerCase()
    .replace(/[`*~]/g, '')
    .replace(/[^\p{L}\p{N}\s_-]/gu, '')
    .replace(/\s/g, '-');
}

const anchorCache = new Map();
function anchors(path) {
  if (!anchorCache.has(path)) {
    const found = new Set();
    for (const line of readFileSync(path, 'utf8').split('\n')) {
      const match = /^#{1,6}\s+(.+)$/.exec(line);
      if (match) found.add(slug(match[1]));
    }
    anchorCache.set(path, found);
  }
  return anchorCache.get(path);
}

const problems = [];
const linked = new Set();
const linkPattern = /\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)|^\[(?!\^)[^\]]+\]:\s+(\S+)/gm;

for (const file of files) {
  const text = readFileSync(file, 'utf8').replace(/```[\s\S]*?```/g, '');
  for (const match of text.matchAll(linkPattern)) {
    const target = match[1] ?? match[2];
    if (!target || /^[a-z][a-z0-9+.-]*:/i.test(target) || target.startsWith('mailto:')) continue;
    const [pathPart, fragment] = target.split('#');
    const resolved = pathPart === '' ? file : resolve(dirname(file), pathPart);
    if (!existsSync(resolved)) {
      problems.push(`${relative(root, file)}: broken link ${target}`);
      continue;
    }
    if (resolved.endsWith('.md') && (file.endsWith('AGENTS.md') || file.endsWith('README.md'))) {
      linked.add(resolved);
    }
    if (fragment && resolved.endsWith('.md') && !anchors(resolved).has(fragment)) {
      problems.push(
        `${relative(root, file)}: no heading "${fragment}" in ${relative(root, resolved)}`,
      );
    }
  }
}

for (const name of readdirSync(join(root, 'docs'))) {
  const path = join(root, 'docs', name);
  if (name.endsWith('.md') && !linked.has(path)) {
    problems.push(`docs/${name} is not linked from AGENTS.md or README.md`);
  }
}

const publicDir = join(root, 'docs', 'public');
for (const name of readdirSync(publicDir)) {
  if (!name.endsWith('.mdx')) continue;
  const text = readFileSync(join(publicDir, name), 'utf8');
  const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(text);
  if (!frontmatter) {
    problems.push(`docs/public/${name} has no frontmatter`);
    continue;
  }
  for (const key of ['title', 'description', 'order']) {
    if (!new RegExp(`^${key}:`, 'm').test(frontmatter[1])) {
      problems.push(`docs/public/${name} frontmatter lacks ${key}`);
    }
  }
}

if (problems.length > 0) {
  console.error(problems.join('\n'));
  console.error(`\n${problems.length} documentation problem(s).`);
  process.exit(1);
}
console.log(`documentation links resolve and every docs/*.md is indexed (${files.length} files).`);
