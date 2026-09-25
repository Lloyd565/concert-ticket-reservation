import { assertCapacityCoversReserved, assertSeatsAvailable } from './capacity';
import {
  CapacityBelowReservedError,
  NotEnoughSeatsError,
} from './concert.errors';

describe('capacity rule', () => {
  it('allows a request that exactly fills the concert', () => {
    expect(() => assertSeatsAvailable(100, 98, 2)).not.toThrow();
  });

  it('rejects a request one seat over capacity and reports seats left', () => {
    expect(() => assertSeatsAvailable(100, 99, 2)).toThrow(NotEnoughSeatsError);
    expect(() => assertSeatsAvailable(100, 99, 2)).toThrow(
      'Not enough seats: 1 left',
    );
  });

  it('rejects any request on a sold-out concert', () => {
    expect(() => assertSeatsAvailable(10, 10, 1)).toThrow(NotEnoughSeatsError);
  });

  it('lets capacity shrink down to, but not below, what is reserved', () => {
    expect(() => assertCapacityCoversReserved(50, 50)).not.toThrow();
    expect(() => assertCapacityCoversReserved(49, 50)).toThrow(
      CapacityBelowReservedError,
    );
  });
});
