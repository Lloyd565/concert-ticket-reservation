import { DomainError } from '../../../common/domain/domain-error';

export class ConcertNotFoundError extends DomainError {
  readonly kind = 'NOT_FOUND';

  constructor() {
    super('Concert not found');
  }
}

export class NotEnoughSeatsError extends DomainError {
  readonly kind = 'CONFLICT';

  constructor(readonly seatsLeft: number) {
    super(`Not enough seats: ${seatsLeft} left`);
  }
}

export class CapacityBelowReservedError extends DomainError {
  readonly kind = 'CONFLICT';

  constructor(readonly reserved: number) {
    super(
      `Capacity cannot be lower than the ${reserved} seats already reserved`,
    );
  }
}

export class ConcertHasActiveReservationsError extends DomainError {
  readonly kind = 'CONFLICT';

  constructor() {
    super('Concert has active reservations');
  }
}
