import { z } from 'zod';

/**
 * Kaana's frozen wire contract measures strings in UTF-16 code units, as Go's
 * utf16Length does. Zod >=4.5 measures its built-in string bounds in code points.
 * These per-schema checks retain the standard bound metadata used by the Go
 * descriptor and JSON Schema generators, with the contract's explicit measure.
 * No Zod constructor, prototype, shared check or array validation is changed.
 */
export function minUtf16Length(minimum: number): z.core.$ZodCheck<string> {
  const check = z.minLength(minimum);
  check._zod.check = (payload) => {
    if (payload.value.length >= minimum) return;
    payload.issues.push({
      origin: 'string',
      code: 'too_small',
      minimum,
      inclusive: true,
      input: payload.value,
      inst: check,
      continue: true,
    });
  };
  return check;
}

export function maxUtf16Length(maximum: number): z.core.$ZodCheck<string> {
  const check = z.maxLength(maximum);
  check._zod.check = (payload) => {
    if (payload.value.length <= maximum) return;
    payload.issues.push({
      origin: 'string',
      code: 'too_big',
      maximum,
      inclusive: true,
      input: payload.value,
      inst: check,
      continue: true,
    });
  };
  return check;
}

export function utf16Length(length: number): z.core.$ZodCheck<string> {
  const check = z.length(length);
  check._zod.check = (payload) => {
    if (payload.value.length === length) return;
    payload.issues.push({
      origin: 'string',
      ...(payload.value.length > length
        ? { code: 'too_big' as const, maximum: length }
        : { code: 'too_small' as const, minimum: length }),
      inclusive: true,
      exact: true,
      input: payload.value,
      inst: check,
      continue: true,
    });
  };
  return check;
}
