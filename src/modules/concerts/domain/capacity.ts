import {
  CapacityBelowReservedError,
  NotEnoughSeatsError,
} from './concert.errors';

// The core invariant: for a concert, the sum of ACTIVE reservation quantities
// never exceeds capacity. These checks are only sound when `reserved` was read
// after locking the concert row (see ConcertRepository.findByIdForUpdate).

/** @throws NotEnoughSeatsError */
export function assertSeatsAvailable(
  capacity: number,
  reserved: number,
  requested: number,
): void {
  if (reserved + requested > capacity) {
    throw new NotEnoughSeatsError(Math.max(capacity - reserved, 0));
  }
}

/** @throws CapacityBelowReservedError */
export function assertCapacityCoversReserved(
  capacity: number,
  reserved: number,
): void {
  if (capacity < reserved) throw new CapacityBelowReservedError(reserved);
}
