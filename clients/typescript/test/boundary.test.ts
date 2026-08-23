/**
 * The boundary gate: this client cannot name an amount.
 *
 * Kaana measures units and never prices them, and ADR 0006 puts every
 * customer-facing amount, receipt and ledger record in Oxy. The Go side asserts
 * the import direction rather than leaving it to review
 * (`TestTheContractCannotReachAnAmount`); this is the same assertion for the
 * client, because the moment a published client exports a money type, a price on
 * a completion is one field away and it will look like Kaana's answer.
 *
 * The forbidden vocabulary is DERIVED from the published contract's own money,
 * price, billing, entitlement and settlement modules rather than typed here. A
 * list somebody typed goes stale the day the contract adds a shape, and a gate
 * that only refuses what somebody remembered is not a gate.
 *
 * Two of those modules genuinely mix Kaana's vocabulary with Oxy's — a token
 * count and a currency amount live in `money`, and a usage report and a receipt
 * live in `usage`. The names on Kaana's side of that line are listed in
 * {@link ALLOWED}, individually, with a reason and an exact count, and every one
 * is asserted to still exist upstream: an allowance for a name that has been
 * renamed away is an allowance excusing nothing.
 */

import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { describe, it } from 'node:test';

const PACKAGE_ROOT = resolve(import.meta.dirname, '..', '..');
const SOURCE_DIR = join(PACKAGE_ROOT, 'src');
const CONTRACTS_INDEX = join(
  PACKAGE_ROOT,
  'node_modules',
  '@oxyhq',
  'contracts',
  'dist',
  'types',
  'index.d.ts',
);

/**
 * The published modules that carry an amount, a price, a plan or a settlement
 * record. Everything they export is forbidden here unless {@link ALLOWED} says
 * why it is not.
 */
const OXY_OWNED_MODULES = [
  './inference/money',
  './inference/priceVersion',
  './inference/accountBilling',
  './inference/entitlement',
  './inference/usage',
] as const;

/**
 * Names those modules export that are Kaana's, not Oxy's.
 *
 * A unit is a measurement and a report is what Kaana produced; a currency, a
 * price, a receipt, a refund and a reservation are the control plane's.
 */
const ALLOWED: Readonly<Record<string, string>> = {
  USAGE_UNITS: 'the units inference is metered in — a measurement, not an amount',
  usageUnitSchema: 'the same, as a schema',
  UsageUnit: 'the same, as a type',
  USAGE_SOURCES: 'whether a quantity was reported, measured or estimated',
  usageSourceSchema: 'the same, as a schema',
  UsageSource: 'the same, as a type',
  usageQuantitySchema: 'a quantity of one unit, which carries no price and no currency',
  UsageQuantity: 'the same, as a type',
  normalizedUsageReportSchema: "the data plane's own technical account of one request",
  NormalizedUsageReport: 'the same, as a type',
  inferenceRequestOutcomeSchema: 'how a request ended, which is on that report',
  InferenceRequestOutcome: 'the same, as a type',
};

/** Reads the export lists the published index declares for one module. */
function exportedBy(index: string, module: string): string[] {
  const pattern = new RegExp(
    String.raw`export\s+(?:type\s+)?\{([^}]*)\}\s+from\s+'${module.replace('.', '\\.')}'`,
    'g',
  );
  const names: string[] = [];
  for (const match of index.matchAll(pattern)) {
    for (const name of (match[1] ?? '').split(',')) {
      const trimmed = name.trim();
      if (trimmed !== '') names.push(trimmed);
    }
  }
  return names;
}

/** Strips comments so a census over source cannot be satisfied by prose. */
function withoutComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

function sourceFiles(): { name: string; code: string }[] {
  return readdirSync(SOURCE_DIR)
    .filter((name) => name.endsWith('.ts'))
    .sort()
    .map((name) => ({ name, code: withoutComments(readFileSync(join(SOURCE_DIR, name), 'utf8')) }));
}

/** Every name this package imports from the published contract. */
function importedFromContracts(files: readonly { name: string; code: string }[]): Set<string> {
  const imported = new Set<string>();
  for (const file of files) {
    for (const match of file.code.matchAll(
      /import\s+(?:type\s+)?\{([^}]*)\}\s+from\s+'@oxyhq\/contracts'/g,
    )) {
      for (const clause of (match[1] ?? '').split(',')) {
        const name = clause.trim().replace(/^type\s+/, '');
        if (name !== '') imported.add(name);
      }
    }
  }
  return imported;
}

const index = readFileSync(CONTRACTS_INDEX, 'utf8');
const oxyOwnedNames = new Set(OXY_OWNED_MODULES.flatMap((module) => exportedBy(index, module)));
const forbidden = new Set([...oxyOwnedNames].filter((name) => !(name in ALLOWED)));

describe('the derivation of the forbidden vocabulary', () => {
  it('read the published modules it claims to', () => {
    // The positive control. "No forbidden name was found" is also what a broken
    // regex, a moved file or a renamed module reports.
    assert.ok(
      oxyOwnedNames.size >= 40,
      `the scan derived only ${oxyOwnedNames.size} names from ${OXY_OWNED_MODULES.length} modules; it is not reading the published index`,
    );
    for (const anchor of ['moneySchema', 'unitPriceSchema', 'usageReceiptSchema', 'billingProfileSchema', 'productPlanSchema']) {
      assert.ok(forbidden.has(anchor), `the derived set is missing ${anchor}, so it is not reading every module`);
    }
  });

  it('allows exactly the names that are measurements rather than amounts', () => {
    // An exact count, not a floor: a floor erodes one defensible line at a time.
    assert.equal(Object.keys(ALLOWED).length, 12);
    for (const name of Object.keys(ALLOWED)) {
      assert.ok(
        oxyOwnedNames.has(name),
        `${name} is allowed here but the published contract no longer exports it from one of those modules — delete the allowance`,
      );
    }
  });
});

describe('the client cannot name an amount', () => {
  const files = sourceFiles();
  const imported = importedFromContracts(files);

  it('scanned the source it claims to', () => {
    assert.ok(files.length >= 6, `the scan read ${files.length} source files; it is not reading src/`);
    assert.ok(
      imported.has('inferenceRequestSchema'),
      'the scan found no import of inferenceRequestSchema, which src/envelope.ts certainly has: it is not reading imports',
    );
  });

  it('imports no money, price, billing, entitlement or settlement shape', () => {
    const violations = [...imported].filter((name) => forbidden.has(name));

    assert.deepEqual(
      violations,
      [],
      `the client imports ${violations.join(', ')}; Kaana measures units and never prices them`,
    );
  });

  it('re-exports nothing from the contract, so nothing reaches a consumer through it', () => {
    // A single `export * from '@oxyhq/contracts'` would put every shape above on
    // this package's surface while importing none of them, which the check
    // before this one cannot see.
    for (const file of files) {
      assert.equal(
        /export\s+\*\s+from/.test(file.code),
        false,
        `src/${file.name} has a star re-export`,
      );
      assert.equal(
        /export\s+(?:type\s+)?\{[^}]*\}\s+from\s+'@oxyhq\/contracts'/.test(file.code),
        false,
        `src/${file.name} re-exports from @oxyhq/contracts`,
      );
    }
  });

  it('exports a surface that names none of them either', () => {
    const surface = files.find((file) => file.name === 'index.ts');
    assert.ok(surface !== undefined);

    const exported = [...surface.code.matchAll(/export\s+\{([^}]*)\}\s+from\s+'\.\/[^']+'/g)]
      .flatMap((match) => (match[1] ?? '').split(','))
      .map((clause) => clause.trim().replace(/^type\s+/, ''))
      .filter((name) => name !== '');

    assert.ok(exported.length >= 30, `the public surface parsed as ${exported.length} names; it is not being read`);
    assert.deepEqual(exported.filter((name) => forbidden.has(name)), []);
  });
});
