/**
 * What went wrong, in business terms. The HTTP status for each kind is decided
 * in exactly one place (DomainExceptionFilter), so domain code never learns
 * about HTTP and services never pick status codes.
 */
export type DomainErrorKind = 'NOT_FOUND' | 'CONFLICT' | 'UNAUTHORIZED';

export abstract class DomainError extends Error {
  abstract readonly kind: DomainErrorKind;

  constructor(message: string) {
    super(message);
    this.name = new.target.name;
  }
}
