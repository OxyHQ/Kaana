import { z } from 'zod';

/** Preserve the existing UTC wire spelling, including minute precision. */
export function utcTimestampCheck(): z.core.$ZodCheck<string> {
  // Zod >=4.6 requires seconds on a qualified datetime. Compose its calendar
  // and time validators explicitly to retain the contract's previous grammar,
  // without accepting offsets, local datetimes or invalid calendar dates.
  const date = z.regexes.date.source.slice(1, -1);
  const time = z.regexes.time({}).source.slice(1, -1);
  return new z.core.$ZodISODateTime({
    type: 'string',
    format: 'datetime',
    check: 'string_format',
    offset: false,
    local: false,
    precision: null,
    pattern: new RegExp(`^${date}T(?:${time}(?:Z))$`),
  });
}
