import {
  CapacityBelowReservedError,
  NotEnoughSeatsError,
} from './concert.errors';

export function assertSeatsAvailable(
  capacity: number,
  reserved: number,
  requested: number,
): void {
  if (reserved + requested > capacity) {
    throw new NotEnoughSeatsError(Math.max(capacity - reserved, 0));
  }
}

export function assertCapacityCoversReserved(
  capacity: number,
  reserved: number,
): void {
  if (capacity < reserved) throw new CapacityBelowReservedError(reserved);
}
