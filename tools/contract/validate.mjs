/**
 * Parses Kaana-produced wire fixtures with the PUBLISHED Zod schemas.
 *
 * The Go descriptor test proves Kaana's structure matches the contract. This
 * proves its values do — a timestamp spelling, a model-reference grammar, a
 * retryability rule, a duplicate unit — by handing each fixture to the
 * contract's own parser rather than to a re-implementation of it. A test that
 * re-implements the code under test measures the re-implementation.
 *
 * It refuses to report success unless:
 *
 *  - it read at least one fixture from each directory (an empty run and a clean
 *    run otherwise look identical);
 *  - every valid fixture PARSED;
 *  - every invalid fixture was REJECTED. These are the vacuity floor: a
 *    validator with a broken schema lookup, a swallowed exception or an
 *    always-true parse would pass the valid ones and fail here.
 *
 * Fixture generation is intentionally isolated from the repository: this
 * command asks the focused Go test to write into a fresh OS temporary
 * directory, validates it, and removes it before exiting.
 *
 * Usage: `bun install --frozen-lockfile && bun run validate`.
 */
import { mkdtempSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

import * as contracts from '@oxy.so/contracts';

const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..');
const TEMP_ROOT = mkdtempSync(join(tmpdir(), 'kaana-contract-'));
const FIXTURE_ROOT = join(TEMP_ROOT, 'wire');

function generateFixtures() {
  const result = spawnSync(
    'go',
    ['test', './internal/contract', '-run', '^TestWriteWireFixtures$', '-count=1'],
    {
      cwd: REPOSITORY_ROOT,
      env: { ...process.env, KAANA_CONTRACT_FIXTURE_DIR: FIXTURE_ROOT },
      stdio: 'inherit',
    },
  );
  if (result.error) {
    throw new Error(`cannot generate contract fixtures: ${result.error.message}`);
  }
  if (result.status !== 0) {
    throw new Error(`contract fixture generation exited with status ${result.status}`);
  }
}

function readFixtures(kind) {
  const dir = join(FIXTURE_ROOT, kind);
  let entries;
  try {
    entries = readdirSync(dir).filter((name) => name.endsWith('.json')).sort();
  } catch (error) {
    throw new Error(`cannot read generated fixtures from ${dir}: ${error.message}`);
  }
  return entries.map((name) => ({
    file: join(kind, name),
    ...JSON.parse(readFileSync(join(dir, name), 'utf8')),
  }));
}

function schemaFor(name) {
  const schema = contracts[name];
  if (schema === undefined || typeof schema.safeParse !== 'function') {
    throw new Error(`the published package exports no schema named ${name}`);
  }
  return schema;
}

try {
  generateFixtures();

  const failures = [];
  const valid = readFixtures('valid');
  const invalid = readFixtures('invalid');

  if (valid.length === 0) {
    throw new Error('no valid fixtures were found; every check below would pass vacuously');
  }
  if (invalid.length === 0) {
    throw new Error('no invalid control fixtures were found; the validator would have no vacuity floor');
  }

  for (const fixture of valid) {
    const result = schemaFor(fixture.schema).safeParse(fixture.value);
    if (!result.success) {
      failures.push(
        `REJECTED a shape Kaana produces — ${fixture.file} (${fixture.schema}/${fixture.case})\n` +
          result.error.issues
            .map((issue) => `      ${issue.path.join('.') || '<root>'}: ${issue.message}`)
            .join('\n'),
      );
    }
  }

  for (const fixture of invalid) {
    const result = schemaFor(fixture.schema).safeParse(fixture.value);
    if (result.success) {
      failures.push(
        `ACCEPTED a control that must be rejected — ${fixture.file} (${fixture.schema}/${fixture.case}); ` +
          'the validator is not reading the schema it claims to',
      );
    }
  }

  process.stdout.write(
    `validated ${valid.length} produced shapes and ${invalid.length} rejection controls ` +
      `against @oxy.so/contracts ${contracts.INFERENCE_CONTRACT_VERSION}\n`,
  );

  if (failures.length > 0) {
    process.stderr.write(`\n${failures.map((failure) => `  - ${failure}`).join('\n\n')}\n\n`);
    process.exitCode = 1;
  }
} finally {
  rmSync(TEMP_ROOT, { recursive: true, force: true });
}
