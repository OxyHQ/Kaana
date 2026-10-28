import { describe, expect, it } from 'bun:test';
import {
  base64DecodedLength,
  checkImageInput,
  INPUT_IMAGE_MEDIA_TYPES,
  type InferenceMessage,
  MAX_INPUT_IMAGE_BYTES,
  MAX_INPUT_IMAGES,
} from '../index.js';
import {
  authorizationRequestSchema,
  declaredCatalogueSchema,
  declaredModelSchema,
} from '../internal.js';
import { requestId } from './fixtures.js';

const PIXEL_PNG =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';

const image = (mediaType = 'image/png', data = PIXEL_PNG) => ({
  type: 'image' as const,
  source: { kind: 'inline' as const, mediaType, data },
});

const user = (...content: InferenceMessage['content']): InferenceMessage => ({
  role: 'user',
  content,
});

describe('checkImageInput', () => {
  it('counts every image and finds nothing wrong with inline PNG, JPEG, WebP and GIF on user and tool messages', () => {
    const messages: InferenceMessage[] = [
      user(
        { type: 'text', text: 'what is on this page?' },
        ...INPUT_IMAGE_MEDIA_TYPES.map((type) => image(type)),
      ),
      {
        role: 'assistant',
        content: [],
        toolCalls: [{ id: 'call_1', name: 'screenshot', arguments: '{}' }],
      },
      {
        role: 'tool',
        toolCallId: 'call_1',
        content: [{ type: 'text', text: 'the tab' }, image('image/jpeg')],
      },
    ];
    expect(checkImageInput(messages)).toEqual({ images: 5, issues: [] });
  });

  it('carries no image, no count and no issue for a text conversation', () => {
    expect(checkImageInput([user({ type: 'text', text: 'hello' })])).toEqual({
      images: 0,
      issues: [],
    });
  });

  it('refuses an image URL: nothing fetches one and none is forwarded', () => {
    const { issues } = checkImageInput([
      user({
        type: 'image',
        source: { kind: 'url', url: 'http://169.254.169.254/latest/meta-data' },
      }),
    ]);
    expect(issues).toEqual([
      expect.objectContaining({ messageIndex: 0, partIndex: 0, field: 'source' }),
    ]);
  });

  it('refuses a format no route reads, malformed base64 and an image past the byte bound', () => {
    const oversized = 'A'.repeat(((MAX_INPUT_IMAGE_BYTES + 3) / 3) * 4);
    expect(base64DecodedLength(oversized)).toBeGreaterThan(MAX_INPUT_IMAGE_BYTES);
    const atBound = 'A'.repeat((MAX_INPUT_IMAGE_BYTES / 3) * 4);
    expect(base64DecodedLength(atBound)).toBe(MAX_INPUT_IMAGE_BYTES);
    const { issues } = checkImageInput([
      user(
        image('image/tiff'),
        image('image/png', 'not base64!'),
        image('image/png', `${PIXEL_PNG}\n`),
        image('image/png', oversized),
        image('image/png', atBound),
      ),
    ]);
    expect(issues.map((issue) => [issue.partIndex, issue.field])).toEqual([
      [0, 'source.mediaType'],
      [1, 'source.data'],
      [2, 'source.data'],
      [3, 'source.data'],
    ]);
  });

  it('refuses an image on a message no provider lets carry one', () => {
    const { issues } = checkImageInput([
      { role: 'system', content: [image()] },
      { role: 'assistant', content: [image()] },
    ]);
    expect(issues.map((issue) => [issue.messageIndex, issue.field])).toEqual([
      [0, 'role'],
      [1, 'role'],
    ]);
  });

  it('admits MAX_INPUT_IMAGES across the conversation and refuses one more', () => {
    const many = Array.from({ length: MAX_INPUT_IMAGES }, () => user(image()));
    expect(checkImageInput(many)).toEqual({ images: MAX_INPUT_IMAGES, issues: [] });
    const over = checkImageInput([...many, user(image())]);
    expect(over.images).toBe(MAX_INPUT_IMAGES + 1);
    expect(over.issues).toEqual([
      expect.objectContaining({ field: 'count', messageIndex: MAX_INPUT_IMAGES }),
    ]);
  });

  it('never repeats the image in an issue', () => {
    const marker = Buffer.from('MARKER-IMAGE-BYTES-0123456789').toString('base64');
    const { issues } = checkImageInput([
      { role: 'assistant', content: [image('image/bmp', marker)] },
      user({ type: 'image', source: { kind: 'url', url: `https://example.com/${marker}` } }),
    ]);
    expect(issues.length).toBeGreaterThan(0);
    expect(JSON.stringify(issues)).not.toContain(marker);
  });
});

describe('a declared model bounds the image input it takes', () => {
  const declared = declaredCatalogueSchema.parse({ schemaVersion: 1, models: [] });

  const base = {
    modelId: 'openai/gpt-5.4-mini',
    publisher: { slug: 'openai', displayName: 'OpenAI' },
    displayName: 'GPT-5.4 mini',
    capabilities: {
      inputModalities: ['image', 'text'],
      outputModalities: ['text'],
      tools: true,
      parallelToolCalls: true,
      structuredOutput: true,
      jsonMode: true,
      reasoning: true,
      streaming: true,
      promptCaching: true,
      maxContextTokens: 400000,
      maxOutputTokens: 128000,
    },
    license: {
      licenseId: 'LicenseRef-Kaana-Serving-Provider-Terms',
      displayName: "Serving provider's terms (not individually reviewed)",
      commercialUseAllowed: false,
      requiresAttribution: true,
    },
    provenance: { releaseKind: 'third_party_hosted' },
    regions: [],
    dataPolicy: {
      retainsPayloads: false,
      retentionDays: 0,
      trainsOnCustomerData: false,
      zeroDataRetentionAvailable: true,
      subprocessors: [],
    },
    deprecation: { status: 'active' },
    evaluations: [],
    audience: 'platform_internal',
  };

  it('parses a line with and without imageInput, and an empty catalogue', () => {
    expect(declared.models).toEqual([]);
    expect(declaredModelSchema.safeParse(base).success).toBe(true);
    const bounded = declaredModelSchema.parse({
      ...base,
      imageInput: { maxInputTokensPerImage: 7374 },
    });
    expect(bounded.imageInput).toEqual({ maxInputTokensPerImage: 7374 });
  });

  it('refuses a bound on a line that takes no image, a zero bound and an unknown member', () => {
    const textOnly = { ...base, capabilities: { ...base.capabilities, inputModalities: ['text'] } };
    expect(
      declaredModelSchema.safeParse({ ...textOnly, imageInput: { maxInputTokensPerImage: 1 } })
        .success,
    ).toBe(false);
    expect(
      declaredModelSchema.safeParse({ ...base, imageInput: { maxInputTokensPerImage: 0 } }).success,
    ).toBe(false);
    expect(
      declaredModelSchema.safeParse({
        ...base,
        imageInput: { maxInputTokensPerImage: 1, maxImages: 4 },
      }).success,
    ).toBe(false);
  });
});

describe('the grant fact inputImages', () => {
  const authorization = {
    schemaVersion: 1,
    kind: 'inference',
    credential: 'oxy_sk_test',
    requestId,
    operation: 'responses',
    facts: { estimatedInputTokens: 10, declaredTools: 0, stream: false },
    bodySha256: 'a'.repeat(64),
    client: { endpoint: '/v1/responses', receivedAt: '2026-10-28T00:00:00.000Z' },
  };
  const withImages = (inputImages: number) => ({
    ...authorization,
    facts: { ...authorization.facts, inputImages },
  });

  it('is optional and counts 1 to MAX_INPUT_IMAGES images', () => {
    expect(authorizationRequestSchema.safeParse(authorization).success).toBe(true);
    expect(authorizationRequestSchema.safeParse(withImages(1)).success).toBe(true);
    expect(authorizationRequestSchema.safeParse(withImages(MAX_INPUT_IMAGES)).success).toBe(true);
    expect(authorizationRequestSchema.safeParse(withImages(0)).success).toBe(false);
    expect(authorizationRequestSchema.safeParse(withImages(MAX_INPUT_IMAGES + 1)).success).toBe(
      false,
    );
  });
});
