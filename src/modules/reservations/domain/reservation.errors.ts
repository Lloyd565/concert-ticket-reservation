import { DomainError } from '../../../common/domain/domain-error';

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
