/** Validate private Auto values and signed negotiation against the workspace contract's schemas. */
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

import * as internalContract from '../../packages/contracts/src/internal.ts';
import * as publicContract from '../../packages/contracts/src/index.ts';

const manifest = JSON.parse(
  readFileSync(new URL('../../packages/contracts/package.json', import.meta.url), 'utf8'),
);
const contracts = { ...publicContract, ...internalContract };
for (const name of [
  'privateAutoSourceApprovalSchema',
  'privateAutoExecutionSchema',
  'privateAutoPrincipalSchema',
  'privateAutoInputSchema',
  'privateAutoInferenceRequestSchema',
]) {
  if (typeof contracts[name]?.safeParse !== 'function')
    throw new Error(`The contract omits ${name}`);
}
const temporary = mkdtempSync(join(tmpdir(), 'kaana-private-contract-'));
const wire = join(temporary, 'wire');
const negotiated = join(temporary, 'negotiated.json');
const repo = resolve(dirname(new URL(import.meta.url).pathname), '../..');
const run = (args, extraEnv) => {
  const result = spawnSync('go', args, {
    cwd: repo,
    env: { ...process.env, ...extraEnv },
    stdio: 'inherit',
  });
  if (result.error || result.status !== 0)
    throw new Error(`Contract fixture command failed: ${result.status}`);
};
try {
  run(['test', './internal/contract', '-run', '^TestWritePrivateAutoWireFixtures$', '-count=1'], {
    KAANA_CONTRACT_FIXTURE_DIR: wire,
  });
  run(
    [
      'test',
      './internal/httpapi',
      '-run',
      '^TestPrivateAutoNegotiationIndependentAndSourceBound$',
      '-count=1',
    ],
    { KAANA_PRIVATE_AUTO_NEGOTIATION_FIXTURE: negotiated },
  );
  const expected = { valid: 6, invalid: 8 };
  for (const kind of ['valid', 'invalid']) {
    const entries = readdirSync(join(wire, kind))
      .filter((name) => name.endsWith('.json'))
      .sort();
    if (entries.length !== expected[kind]) throw new Error(`Unexpected ${kind} fixture count`);
    for (const name of entries) {
      const fixture = JSON.parse(readFileSync(join(wire, kind, name), 'utf8'));
      const parsed = contracts[fixture.schema]?.safeParse(fixture.value);
      if (!parsed || parsed.success !== (kind === 'valid')) {
        // Only schema/case names are diagnostic; transient input is never printed.
        throw new Error(`Contract ${kind} control disagrees: ${fixture.schema}/${fixture.case}`);
      }
    }
  }
  const descriptor = JSON.parse(readFileSync(negotiated, 'utf8'));
  if (
    descriptor.privateAutoExecutionContractVersion !== '3.7.0' ||
    descriptor.scopedExecutionContractVersion !== undefined ||
    !descriptor.snapshotId ||
    descriptor.deployments.length !== 1
  ) {
    throw new Error('Independent negotiation identity differs');
  }
  const row = descriptor.deployments[0];
  const approval = contracts.privateAutoSourceApprovalSchema.parse(row.privateAutoSourceApproval);
  if (
    row.scopedExecution !== undefined ||
    row.deploymentId !== approval.deploymentId ||
    row.provider !== approval.provider ||
    row.modelReference !== approval.modelReference ||
    row.upstreamModelId !== approval.upstreamModelId ||
    JSON.stringify(row.regions) !== JSON.stringify(approval.regions)
  ) {
    throw new Error('Actual negotiated descriptor differs from its shared source approval');
  }
  console.log(
    JSON.stringify({
      kind: 'private-auto-shared-contract-validation',
      package: manifest.name,
      packageVersion: manifest.version,
      privateContractVersion: '3.7.0',
      valid: 6,
      rejected: 8,
      actualSignedNegotiatedDescriptor: 1,
    }),
  );
} finally {
  rmSync(temporary, { recursive: true, force: true });
}
