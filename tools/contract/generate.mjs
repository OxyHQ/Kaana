/**
 * Derives `internal/contract/descriptor.json` from the PUBLISHED
 * `@oxyhq/contracts` package.
 *
 * The descriptor is the machine-readable statement of what the Oxy↔data-plane
 * inference contract actually says: every wire shape, every field, its
 * optionality, its kind, its enum members, its literal values and the string
 * constraints that carry meaning. `internal/contract/contract_test.go` compares
 * Kaana's Go types against it field by field, so a change on either side is a
 * red test rather than a runtime surprise.
 *
 * Two properties make it a gate rather than a snapshot:
 *
 *  1. The set of shapes is DISCOVERED, not listed. Every module under
 *     `dist/esm/inference/` is imported and every export it names is described.
 *     A shape added to the contract therefore appears here on the next
 *     regeneration, and the Go side fails until it is mapped or explicitly
 *     recorded as not applicable to the data plane.
 *  2. The package version is pinned exactly in `package.json`, so regeneration
 *     is deterministic. CI regenerates and asserts the committed file is
 *     unchanged, which is what catches a hand-edited descriptor and a version
 *     bump that nobody re-derived.
 *
 * Usage: `bun install --frozen-lockfile && bun run generate` in this directory.
 */
import { createRequire } from 'node:module';
import { readdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const require = createRequire(import.meta.url);
const packageJsonPath = require.resolve('@oxyhq/contracts/package.json');
const packageRoot = dirname(packageJsonPath);
const packageManifest = require('@oxyhq/contracts/package.json');
const inferenceDir = join(packageRoot, 'dist', 'esm', 'inference');

const OUTPUT = resolve(import.meta.dirname, '..', '..', 'internal', 'contract', 'descriptor.json');

/* -------------------------------------------------------------------------- */
/*  Discovery                                                                 */
/* -------------------------------------------------------------------------- */

/**
 * Every export of every module under the package's `inference/` directory.
 *
 * Imported by filesystem path rather than by package specifier: the package's
 * `exports` map publishes only the root, so a subpath import would be refused,
 * and enumerating the root's exports would mix inference shapes with the rest
 * of the contracts package.
 */
async function loadInferenceExports() {
  const moduleFiles = readdirSync(inferenceDir)
    .filter((name) => name.endsWith('.js'))
    .sort();
  if (moduleFiles.length === 0) {
    throw new Error(`no inference modules found under ${inferenceDir}`);
  }
  /** @type {Map<string, {value: unknown, module: string}>} */
  const exports = new Map();
  for (const file of moduleFiles) {
    const loaded = await import(pathToFileURL(join(inferenceDir, file)).href);
    for (const [name, value] of Object.entries(loaded)) {
      if (exports.has(name)) {
        throw new Error(`export ${name} is declared by two inference modules`);
      }
      exports.set(name, { value, module: file.replace(/\.js$/, '') });
    }
  }
  return exports;
}

const isZodSchema = (value) =>
  typeof value === 'object' && value !== null && '_def' in value && typeof value.parse === 'function';

/* -------------------------------------------------------------------------- */
/*  Description                                                               */
/* -------------------------------------------------------------------------- */

/** Zod wrappers that carry a modifier rather than a shape of their own. */
function unwrapModifiers(schema, modifiers) {
  let current = schema;
  for (;;) {
    const kind = current._def.typeName;
    if (kind === 'ZodOptional') {
      modifiers.optional = true;
      current = current._def.innerType;
    } else if (kind === 'ZodNullable') {
      modifiers.nullable = true;
      current = current._def.innerType;
    } else if (kind === 'ZodDefault') {
      modifiers.hasDefault = true;
      modifiers.defaultValue = current._def.defaultValue();
      current = current._def.innerType;
    } else {
      return current;
    }
  }
}

/** `.superRefine()` and `.brand()` wrap a schema without changing its wire shape. */
function unwrapTransparent(schema, notes) {
  let current = schema;
  for (;;) {
    const kind = current._def.typeName;
    if (kind === 'ZodEffects') {
      notes.refined = true;
      current = current._def.schema;
    } else if (kind === 'ZodBranded') {
      notes.branded = true;
      current = current._def.type;
    } else {
      return current;
    }
  }
}

function describeStringChecks(checks) {
  const constraints = {};
  for (const check of checks ?? []) {
    switch (check.kind) {
      case 'min':
        constraints.minLength = check.value;
        break;
      case 'max':
        constraints.maxLength = check.value;
        break;
      case 'regex':
        constraints.regex = check.regex.source;
        constraints.regexFlags = check.regex.flags;
        break;
      case 'datetime':
        constraints.datetime = true;
        break;
      default:
        constraints[check.kind] = true;
    }
  }
  return constraints;
}

function describeNumberChecks(checks) {
  const constraints = {};
  for (const check of checks ?? []) {
    switch (check.kind) {
      case 'int':
        constraints.int = true;
        break;
      case 'min':
        constraints.min = check.value;
        constraints.minInclusive = check.inclusive;
        break;
      case 'max':
        constraints.max = check.value;
        constraints.maxInclusive = check.inclusive;
        break;
      case 'finite':
        constraints.finite = true;
        break;
      default:
        constraints[check.kind] = true;
    }
  }
  return constraints;
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
  const base = unwrapTransparent(inner, notes);
  Object.assign(node, notes);

  const typeName = base._def.typeName;
  switch (typeName) {
    case 'ZodObject': {
      const shape = base._def.shape();
      const fields = Object.entries(shape)
        .map(([fieldName, fieldSchema]) => ({
          name: fieldName,
          ...describe(fieldSchema, nameOfSchema, true),
        }))
        .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
      return {
        kind: 'object',
        strict: base._def.unknownKeys === 'strict',
        fields,
        ...node,
      };
    }
    case 'ZodArray':
      return {
        kind: 'array',
        items: describe(base._def.type, nameOfSchema, true),
        minItems: base._def.minLength?.value,
        maxItems: base._def.maxLength?.value,
        ...node,
      };
    case 'ZodString':
      return { kind: 'string', constraints: describeStringChecks(base._def.checks), ...node };
    case 'ZodNumber':
      return { kind: 'number', constraints: describeNumberChecks(base._def.checks), ...node };
    case 'ZodBoolean':
      return { kind: 'boolean', ...node };
    case 'ZodLiteral':
      return { kind: 'literal', value: base._def.value, ...node };
    case 'ZodEnum':
      return { kind: 'enum', values: [...base._def.values], ...node };
    case 'ZodDiscriminatedUnion':
      return {
        kind: 'discriminatedUnion',
        discriminator: base._def.discriminator,
        variants: [...base._def.options].map((option) => describe(option, nameOfSchema, true)),
        ...node,
      };
    case 'ZodUnion':
      return {
        kind: 'union',
        variants: [...base._def.options].map((option) => describe(option, nameOfSchema, true)),
        ...node,
      };
    case 'ZodRecord':
      // `keyType`/`valueType` rather than `keys`/`values`: an enum node already
      // uses `values` for its members, and one key meaning two things is how a
      // consumer of this descriptor reads a record as an enum.
      return {
        kind: 'record',
        keyType: describe(base._def.keyType, nameOfSchema, true),
        valueType: describe(base._def.valueType, nameOfSchema, true),
        ...node,
      };
    case 'ZodUnknown':
    case 'ZodAny':
      return { kind: 'unknown', ...node };
    default:
      throw new Error(`unhandled zod type ${typeName}; the generator must learn it before the descriptor can be trusted`);
  }
}

/* -------------------------------------------------------------------------- */
/*  Output                                                                    */
/* -------------------------------------------------------------------------- */

const inferenceExports = await loadInferenceExports();

/** Identity map so a nested use of an exported schema is emitted as a reference. */
const nameOfSchema = new Map();
for (const [name, { value }] of inferenceExports) {
  if (isZodSchema(value)) {
    nameOfSchema.set(value, name);
    // A `.superRefine()`d or branded schema is a different object from the one
    // its fields nest, so both identities must resolve to the same name.
    const notes = {};
    const transparent = unwrapTransparent(value, notes);
    if (transparent !== value && !nameOfSchema.has(transparent)) {
      nameOfSchema.set(transparent, name);
    }
  }
}

const shapes = {};
const constants = {};
for (const [name, { value, module }] of [...inferenceExports].sort(([a], [b]) => (a < b ? -1 : 1))) {
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
    constants[name] = { module, value };
  }
}

const descriptor = {
  $comment:
    'GENERATED by tools/contract/generate.mjs from the published @oxyhq/contracts package. Do not edit by hand: CI regenerates this file and fails on any difference.',
  package: '@oxyhq/contracts',
  packageVersion: packageManifest.version,
  contractVersion: constants.INFERENCE_CONTRACT_VERSION?.value,
  shapeCount: Object.keys(shapes).length,
  constants,
  shapes,
};

if (typeof descriptor.contractVersion !== 'string') {
  throw new Error('INFERENCE_CONTRACT_VERSION is missing from the published package');
}
if (descriptor.shapeCount === 0) {
  throw new Error('no zod schemas were described; the generator read nothing');
}

writeFileSync(OUTPUT, `${JSON.stringify(descriptor, null, 2)}\n`);
process.stdout.write(
  `wrote ${OUTPUT}\n  package ${descriptor.package}@${descriptor.packageVersion}\n  contract ${descriptor.contractVersion}\n  ${descriptor.shapeCount} shapes, ${Object.keys(constants).length} constants\n`,
);
