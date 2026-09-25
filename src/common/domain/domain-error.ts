export type DomainErrorKind = 'NOT_FOUND' | 'CONFLICT' | 'UNAUTHORIZED';

export abstract class DomainError extends Error {
  abstract readonly kind: DomainErrorKind;

  constructor(message: string) {
    super(message);
    this.name = new.target.name;
  }
}
