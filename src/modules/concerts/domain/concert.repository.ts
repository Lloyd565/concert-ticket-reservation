import { Concert, ConcertChanges, NewConcert } from './concert';

export interface Page<T> {
  items: T[];
  total: number;
}

export abstract class ConcertRepository {
  abstract create(concert: NewConcert): Promise<Concert>;
  abstract findById(id: string): Promise<Concert | null>;
  abstract findPage(page: number, limit: number): Promise<Page<Concert>>;
  abstract update(id: string, changes: ConcertChanges): Promise<Concert>;
  abstract delete(id: string): Promise<void>;

  /**
   * SELECT ... FOR UPDATE on the concert row. Every change to a concert's seat
   * accounting takes this lock first, which serialises them. Must be called
   * inside TransactionRunner.run().
   */
  abstract findByIdForUpdate(id: string): Promise<Concert | null>;

  /** Sum of ACTIVE reservation quantities, optionally ignoring one reservation. */
  abstract reservedSeats(
    concertId: string,
    excludeReservationId?: string,
  ): Promise<number>;
}
