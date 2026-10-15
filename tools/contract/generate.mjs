/**
 * Derives `internal/contract/descriptor.json` from the `@kaana.ai/contracts`
 * WORKSPACE source (`packages/contracts/src`), not from npm: the source is the
 * commit (Kaana ADR 0002, decision 10). Direction: Zod → descriptor → Go.
 *
 * The descriptor is the machine-readable statement of what the contract says:
 * every shape, every field, its optionality, its kind, its enum members, its
 * literal values and the string constraints that carry meaning.
 * `internal/contract/descriptor_test.go` compares Kaana's Go types against it
 * field by field, so a change on either side is a red test rather than a
 * runtime surprise.
 *
 * Three properties make it a gate rather than a snapshot:
 *
 *  1. The set of shapes is DISCOVERED, not listed. Every module under
 *     `src/public/` and `src/internal/` is imported and every export it names
 *     is described, tagged with its surface. A shape added to the contract
 *     appears here on the next regeneration, and the Go side fails until it is
 *     mapped or explicitly recorded as not applicable to the data plane.
 *  2. Every module export must be reachable from the package entry point of
 *     its surface (`src/index.ts` for public, `src/internal.ts` for internal),
 *     and every export name is unique across the package, so the descriptor
 *     cannot describe a shape a consumer cannot import, or two shapes under one
 *     name.
 *  3. The input is the commit itself, so regeneration is deterministic. CI
 *     regenerates and asserts the committed file is unchanged, which catches a
 *     hand-edited descriptor and a contract change nobody re-derived.
 *
 * Usage: `bun install --frozen-lockfile && bun run generate` in this directory.
 */
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const PACKAGE_ROOT = resolve(import.meta.dirname, '..', '..', 'packages', 'contracts');
const SOURCE = join(PACKAGE_ROOT, 'src');
const OUTPUT = resolve(import.meta.dirname, '..', '..', 'internal', 'contract', 'descriptor.json');
const SURFACES = { public: 'index.ts', internal: 'internal.ts' };

const manifest = JSON.parse(readFileSync(join(PACKAGE_ROOT, 'package.json'), 'utf8'));

/* -------------------------------------------------------------------------- */
/*  Discovery                                                                 */
/* -------------------------------------------------------------------------- */

/** Every export of every module of every surface, checked against its entry point. */
async function loadExports() {
  /** @type {Map<string, {value: unknown, module: string}>} */
  const exports = new Map();
  for (const [surface, entry] of Object.entries(SURFACES)) {
    const directory = join(SOURCE, surface);
    const moduleFiles = readdirSync(directory)
      .filter((name) => name.endsWith('.ts'))
      .sort();
    if (moduleFiles.length === 0) {
      throw new Error(`no ${surface} modules found under ${directory}`);
    }
    const surfaceNames = new Set();
    for (const file of moduleFiles) {
      const loaded = await import(pathToFileURL(join(directory, file)).href);
      const module = `${surface}/${file.replace(/\.ts$/, '')}`;
      for (const [name, value] of Object.entries(loaded)) {
        if (exports.has(name)) {
          throw new Error(
            `export ${name} is declared by ${exports.get(name).module} and ${module}`,
          );
        }
        exports.set(name, { value, module });
        surfaceNames.add(name);
      }
    }
    const entryExports = new Set(
      Object.keys(await import(pathToFileURL(join(SOURCE, entry)).href)),
    );
    const unreachable = [...surfaceNames].filter((name) => !entryExports.has(name));
    const foreign = [...entryExports].filter((name) => !surfaceNames.has(name));
    if (unreachable.length > 0 || foreign.length > 0) {
      throw new Error(
        `src/${entry} must export exactly the ${surface} modules: unreachable [${unreachable.join(', ')}], foreign [${foreign.join(', ')}]`,
      );
    }
  }
  return exports;
}

const isZodSchema = (value) =>
  typeof value === 'object' &&
  value !== null &&
  typeof value._zod === 'object' &&
  typeof value.safeParse === 'function';

/* -------------------------------------------------------------------------- */
/*  Description                                                               */
/* -------------------------------------------------------------------------- */

const defOf = (schema) => schema._zod.def;
const checksOf = (schema) => (defOf(schema).checks ?? []).map((check) => check._zod.def);

/** Zod wrappers that carry a modifier rather than a shape of their own. */
function unwrapModifiers(schema, modifiers) {
  let current = schema;
  for (;;) {
    const def = defOf(current);
    if (def.type === 'optional') {
      modifiers.optional = true;
      current = def.innerType;
    } else if (def.type === 'nullable') {
      modifiers.nullable = true;
      current = def.innerType;
    } else if (def.type === 'default') {
      modifiers.hasDefault = true;
      modifiers.defaultValue =
        typeof def.defaultValue === 'function' ? def.defaultValue() : def.defaultValue;
      current = def.innerType;
    } else {
      return current;
    }
  }
}

/** A refinement (`.refine`/`.superRefine`) is a `custom` check; it changes no wire shape. */
function noteRefinements(schema, notes) {
  if (checksOf(schema).some((check) => check.check === 'custom')) {
    notes.refined = true;
  }
}

function describeStringChecks(schema) {
  const constraints = {};
  // A format schema (`z.guid()`, `z.iso.datetime()`) carries its format on its
  // own definition rather than as a check.
  const own = defOf(schema);
  const formats = own.format === undefined ? [] : [{ check: 'string_format', ...own }];
  for (const check of [...formats, ...checksOf(schema)]) {
    switch (check.check) {
      case 'min_length':
        constraints.minLength = check.minimum;
        break;
      case 'max_length':
        constraints.maxLength = check.maximum;
        break;
      case 'length_equals':
        constraints.length = check.length;
        break;
      case 'string_format':
        if (check.format === 'regex') {
          constraints.regex = check.pattern.source;
          constraints.regexFlags = check.pattern.flags;
        } else if (check.format === 'guid' || check.format === 'uuid') {
          constraints.uuid = true;
        } else {
          constraints[check.format] = true;
        }
        break;
      case 'custom':
        break;
      default:
        constraints[check.check] = true;
    }
  }
  return constraints;
}

function describeNumberChecks(schema) {
  const constraints = {};
  for (const check of checksOf(schema)) {
    switch (check.check) {
      case 'number_format':
        if (check.format === 'safeint' || check.format === 'int32' || check.format === 'uint32') {
          constraints.int = true;
        } else {
          constraints[check.format] = true;
        }
        break;
      case 'greater_than':
        constraints.min = check.value;
        constraints.minInclusive = check.inclusive;
        break;
      case 'less_than':
        constraints.max = check.value;
        constraints.maxInclusive = check.inclusive;
        break;
      case 'custom':
        break;
      default:
        constraints[check.check] = true;
    }
  }
  return constraints;
}

function lengthBounds(schema) {
  const bounds = {};
  for (const check of checksOf(schema)) {
    if (check.check === 'min_length') bounds.minItems = check.minimum;
    if (check.check === 'max_length') bounds.maxItems = check.maximum;
  }
  return bounds;
}

/**
 * @param schema        the zod schema to describe
 * @param nameOfSchema  identity map from an exported schema object to its export name
 * @param allowRef      false at the top of a shape (it is being expanded), true below it
 */
function describe(schema, nameOfSchema, allowRef) {
  const node = {};
  const inner = unwrapModifiers(schema, node);

  if (allowRef) {
    const referenced = nameOfSchema.get(inner);
    if (referenced !== undefined) {
      return { kind: 'ref', ref: referenced, ...node };
    }
  }

  const notes = {};
  noteRefinements(inner, notes);
  Object.assign(node, notes);

  const def = defOf(inner);
  switch (def.type) {
    case 'object': {
      const fields = Object.entries(def.shape)
        .map(([fieldName, fieldSchema]) => ({
          name: fieldName,
          ...describe(fieldSchema, nameOfSchema, true),
        }))
        .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
      return {
        kind: 'object',
        strict: def.catchall !== undefined && defOf(def.catchall).type === 'never',
        fields,
        ...node,
      };
    }
    case 'array':
      return {
        kind: 'array',
        items: describe(def.element, nameOfSchema, true),
        ...lengthBounds(inner),
        ...node,
      };
    case 'string':
      return { kind: 'string', constraints: describeStringChecks(inner), ...node };
    case 'number':
      return { kind: 'number', constraints: describeNumberChecks(inner), ...node };
    case 'boolean':
      return { kind: 'boolean', ...node };
    case 'literal': {
      if (def.values.length !== 1) {
        throw new Error('a multi-value literal has no descriptor form; use an enum');
      }
      return { kind: 'literal', value: def.values[0], ...node };
    }
    case 'enum':
      return { kind: 'enum', values: Object.values(def.entries), ...node };
    case 'union':
      if (def.discriminator !== undefined) {
        return {
          kind: 'discriminatedUnion',
          discriminator: def.discriminator,
          variants: def.options.map((option) => describe(option, nameOfSchema, true)),
          ...node,
        };
      }
      return {
        kind: 'union',
        variants: def.options.map((option) => describe(option, nameOfSchema, true)),
        ...node,
      };
    case 'record':
      // `keyType`/`valueType` rather than `keys`/`values`: an enum node already
      // uses `values` for its members, and one key meaning two things is how a
      // consumer of this descriptor reads a record as an enum.
      return {
        kind: 'record',
        keyType: describe(def.keyType, nameOfSchema, true),
        valueType: describe(def.valueType, nameOfSchema, true),
        ...node,
      };
    case 'unknown':
    case 'any':
      return { kind: 'unknown', ...node };
    default:
      throw new Error(
        `unhandled zod type ${def.type}; the generator must learn it before the descriptor can be trusted`,
      );
  }
}

/** A constant the descriptor can carry: JSON data, never a schema or a function. */
function describeConstant(name, value) {
  if (typeof value === 'function') return undefined;
  const text = JSON.stringify(value, (_key, inner) => {
    if (isZodSchema(inner)) {
      throw new Error(`constant ${name} embeds a zod schema; export the schema instead`);
    }
    return inner;
  });
  return text === undefined ? undefined : JSON.parse(text);
}

/* -------------------------------------------------------------------------- */
/*  Output                                                                    */
/* -------------------------------------------------------------------------- */

const contractExports = await loadExports();

/** Identity map so a nested use of an exported schema is emitted as a reference. */
const nameOfSchema = new Map();
for (const [name, { value }] of contractExports) {
  if (isZodSchema(value)) nameOfSchema.set(value, name);
}

const shapes = {};
const constants = {};
for (const [name, { value, module }] of [...contractExports].sort(([a], [b]) => (a < b ? -1 : 1))) {
  if (isZodSchema(value)) {
    const described = describe(value, nameOfSchema, false);
    shapes[name] = {
      module,
      versioned:
        described.kind === 'object' &&
        described.fields.some((field) => field.name === 'schemaVersion'),
      ...described,
    };
  } else {
    const described = describeConstant(name, value);
    if (described !== undefined) constants[name] = { module, value: described };
  }
}

const descriptor = {
  $comment:
    'GENERATED by tools/contract/generate.mjs from the @kaana.ai/contracts workspace source (packages/contracts/src). Do not edit by hand: CI regenerates this file and fails on any difference.',
  package: manifest.name,
  packageVersion: manifest.version,
  contractVersion: constants.INFERENCE_CONTRACT_VERSION?.value,
  shapeCount: Object.keys(shapes).length,
  constants,
  shapes,
};

if (typeof descriptor.contractVersion !== 'string') {
  throw new Error('INFERENCE_CONTRACT_VERSION is missing from the contract');
}
if (descriptor.shapeCount === 0) {
  throw new Error('no zod schemas were described; the generator read nothing');
}

writeFileSync(OUTPUT, `${JSON.stringify(descriptor, null, 2)}\n`);
process.stdout.write(
  `wrote ${OUTPUT}\n  package ${descriptor.package}@${descriptor.packageVersion} (workspace)\n  contract ${descriptor.contractVersion}\n  ${descriptor.shapeCount} shapes, ${Object.keys(constants).length} constants\n`,
);
