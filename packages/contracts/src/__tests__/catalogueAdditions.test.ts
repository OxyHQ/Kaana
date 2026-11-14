/**
 * Contract set 8.17.0: `CATALOGUE_ADDITIONS` names values of `modelSchema`, and
 * says what Kaana leaves out when one of them is withheld. Each entry is held
 * to the schema here, so a path that names nothing, a member the enum lacks or
 * an `emptied` that would leave a required list empty fails before Kaana
 * renders it. Which entries a deployed release needs is the `@kaana.ai/sdk`
 * compatibility suite's question.
 */
import { describe, expect, it } from 'bun:test';
import { CATALOGUE_ADDITIONS, INFERENCE_CONTRACT_VERSION, modelSchema } from '../index.js';

type Def = {
  type: string;
  innerType?: unknown;
  element?: unknown;
  shape?: Record<string, unknown>;
  entries?: Record<string, string>;
  checks?: { _zod: { def: { check: string; minimum?: number } } }[];
};
const defOf = (schema: unknown): Def => (schema as { _zod: { def: Def } })._zod.def;

/** The schema under its optional/default wrappers, and whether one made it optional. */
function unwrap(schema: unknown): { schema: unknown; optional: boolean } {
  let current = schema;
  let optional = false;
  for (;;) {
    const def = defOf(current);
    if (def.type === 'optional' || def.type === 'default') {
      optional = true;
      current = def.innerType;
    } else {
      return { schema: current, optional };
    }
  }
}

interface Step {
  /** The path of the field this step reached. */
  path: string[];
  optional: boolean;
  /** Set when the field is a list, with the list's minimum length. */
  minItems?: number;
}

/** Each field `path` reaches in `modelSchema`, and the schema at its end. */
function resolve(path: readonly string[]): { steps: Step[]; leaf: unknown } {
  const steps: Step[] = [];
  let node: unknown = modelSchema;
  const crossList = (step: Step): void => {
    const def = defOf(node);
    if (def.type !== 'array') return;
    const min = def.checks?.find((check) => check._zod.def.check === 'min_length');
    step.minItems = min?._zod.def.minimum ?? 0;
    node = unwrap(def.element).schema;
  };
  for (const [index, name] of path.entries()) {
    const def = defOf(node);
    if (def.type !== 'object' || def.shape?.[name] === undefined) {
      throw new Error(`${path.slice(0, index + 1).join('.')} is not a field of modelSchema`);
    }
    const field = unwrap(def.shape[name]);
    node = field.schema;
    const step: Step = { path: path.slice(0, index + 1), optional: field.optional };
    crossList(step);
    steps.push(step);
  }
  return { steps, leaf: node };
}

const semver = (version: string): number[] => version.split('.').map(Number);
const notAfter = (a: string, b: string): boolean => {
  const [x, y] = [semver(a), semver(b)];
  for (let index = 0; index < 3; index += 1) {
    if (x[index] !== y[index]) return (x[index] ?? 0) < (y[index] ?? 0);
  }
  return true;
};

describe('CATALOGUE_ADDITIONS', () => {
  it.each(CATALOGUE_ADDITIONS.map((entry) => [entry.path.join('.'), entry] as const))(
    '%s names a value modelSchema has, introduced by a published set',
    (_name, entry) => {
      expect(entry.since).toMatch(/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
      expect(notAfter(entry.since, INFERENCE_CONTRACT_VERSION)).toBe(true);
      const { steps, leaf } = resolve(entry.path);
      const last = steps.at(-1);
      if (entry.member === undefined) {
        // A field a reader may not be sent is one the contract lets it lack.
        expect(last?.optional).toBe(true);
        expect(entry.emptied).toBeUndefined();
        return;
      }
      const def = defOf(leaf);
      expect(def.type).toBe('enum');
      expect(Object.values(def.entries ?? {})).toContain(entry.member);
      // The element left out is one of a list: the deepest list on the path.
      const list = [...steps].reverse().find((step) => step.minItems !== undefined);
      expect(list).toBeDefined();
      if (list === undefined) return;
      // A list that may be empty stays, empty; one that may not takes the
      // nearest optional field holding it with it.
      const holder =
        (list.minItems ?? 0) === 0
          ? undefined
          : steps
              .slice(0, steps.indexOf(list) + 1)
              .reverse()
              .find((step) => step.optional);
      if ((list.minItems ?? 0) > 0) expect(holder).toBeDefined();
      expect(entry.emptied).toEqual(holder?.path);
    },
  );

  it('names each value once, oldest set first', () => {
    const keys = CATALOGUE_ADDITIONS.map((entry) => `${entry.path.join('.')}=${entry.member}`);
    expect(new Set(keys).size).toBe(keys.length);
    for (const [index, entry] of CATALOGUE_ADDITIONS.entries()) {
      const next = CATALOGUE_ADDITIONS[index + 1];
      if (next !== undefined) expect(notAfter(entry.since, next.since)).toBe(true);
    }
  });

  it('refuses an entry the schema does not have, so the checks above can fail', () => {
    expect(() => resolve(['capabilities', 'realtime', 'nonsense'])).toThrow();
    const { leaf } = resolve(['capabilities', 'realtime', 'sessionKinds']);
    expect(Object.values(defOf(leaf).entries ?? {})).not.toContain('nonsense');
  });
});
