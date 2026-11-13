/**
 * `POST /v1/rerank` — rank documents by relevance to a query.
 *
 * New in contract set 8.11.0, CONTRACT ONLY: these shapes and the `rerank` grant
 * operation are declared so Oxy can support the operation before Kaana serves
 * it. Kaana does not serve the route yet and emits no `rerank` operation.
 *
 * Version 1 takes string documents only. The response carries a relevance score
 * per document and the token usage; it never carries an amount.
 */

import { z } from 'zod';
import { maxUtf16Length, minUtf16Length } from '../stringBounds.js';
import { generationIdSchema, modelReferenceSchema } from './identifiers.js';

/** The most documents one rerank request may carry. */
export const MAX_RERANK_DOCUMENTS = 1000;

export const rerankRequestSchema = z
  .object({
    /** `publisher/model` or `publisher/model@revision`. */
    model: modelReferenceSchema,
    query: z.string().check(minUtf16Length(1)),
    /** String documents only. */
    documents: z
      .array(z.string().check(minUtf16Length(1)))
      .min(1)
      .max(MAX_RERANK_DOCUMENTS),
    /** How many of the most relevant documents to return; all when absent. */
    top_n: z.number().int().min(1).optional(),
    /** Echo each ranked document's text in its result. */
    return_documents: z.boolean().optional(),
    /** The delegated end-user id, attribution only (as on `/v1/embeddings`). */
    user: z.string().check(minUtf16Length(1)).check(maxUtf16Length(64)).optional(),
  })
  .strict();

export const rerankResponseSchema = z
  .object({
    id: generationIdSchema,
    /** The revision that produced the scores. */
    model: modelReferenceSchema,
    /** Most relevant first. */
    results: z
      .array(
        z
          .object({
            /** The position of the document in the request's `documents`. */
            index: z.number().int().nonnegative(),
            relevance_score: z.number(),
            document: z.object({ text: z.string() }).strict().optional(),
          })
          .strict(),
      )
      .max(MAX_RERANK_DOCUMENTS),
    usage: z
      .object({
        prompt_tokens: z.number().int().nonnegative(),
        total_tokens: z.number().int().nonnegative(),
      })
      .strict(),
  })
  .strict()
  .superRefine((response, ctx) => {
    const indexes = response.results.map((result) => result.index);
    if (new Set(indexes).size !== indexes.length) {
      ctx.addIssue({ code: 'custom', path: ['results'], message: 'result indexes must be unique' });
    }
  });

export type RerankRequest = z.infer<typeof rerankRequestSchema>;
export type RerankResponse = z.infer<typeof rerankResponseSchema>;
