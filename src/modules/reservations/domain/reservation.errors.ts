import { DomainError } from '../../../common/domain/domain-error';

// Also used when the reservation exists but belongs to someone else, so a
// caller cannot probe which reservation ids exist.
export class ReservationNotFoundError extends DomainError {
  readonly kind = 'NOT_FOUND';

  constructor() {
    super('Reservation not found');
  }
}

export class ReservationNotActiveError extends DomainError {
  readonly kind = 'CONFLICT';

  constructor() {
    super('Only an ACTIVE reservation can be changed');
  }
}
