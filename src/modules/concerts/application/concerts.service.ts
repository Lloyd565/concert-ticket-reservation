import { Injectable } from '@nestjs/common';
import { TransactionRunner } from '../../../common/application/transaction-runner';
import { assertCapacityCoversReserved } from '../domain/capacity';
import { Concert, ConcertChanges, NewConcert } from '../domain/concert';
import {
  ConcertHasActiveReservationsError,
  ConcertNotFoundError,
} from '../domain/concert.errors';
import { ConcertRepository, Page } from '../domain/concert.repository';

@Injectable()
export class ConcertsService {
  constructor(
    private readonly concerts: ConcertRepository,
    private readonly tx: TransactionRunner,
  ) {}

  list(page: number, limit: number): Promise<Page<Concert>> {
    return this.concerts.findPage(page, limit);
  }

  async get(id: string): Promise<Concert> {
    const concert = await this.concerts.findById(id);
    if (!concert) throw new ConcertNotFoundError();
    return concert;
  }

  create(concert: NewConcert): Promise<Concert> {
    return this.concerts.create(concert);
  }

  update(id: string, changes: ConcertChanges): Promise<Concert> {
    return this.tx.run(async () => {
      const concert = await this.concerts.findByIdForUpdate(id);
      if (!concert) throw new ConcertNotFoundError();
      if (changes.capacity !== undefined) {
        assertCapacityCoversReserved(
          changes.capacity,
          await this.concerts.reservedSeats(id),
        );
      }
      return this.concerts.update(id, changes);
    });
  }

  delete(id: string): Promise<void> {
    return this.tx.run(async () => {
      const concert = await this.concerts.findByIdForUpdate(id);
      if (!concert) throw new ConcertNotFoundError();
      if ((await this.concerts.reservedSeats(id)) > 0) {
        throw new ConcertHasActiveReservationsError();
      }
      await this.concerts.delete(id);
    });
  }
}
