import { Injectable } from '@nestjs/common';
import { DataSource } from 'typeorm';
import {
  currentManager,
  requireTransaction,
} from '../../../common/database/typeorm-transaction-runner';
import { ReservationStatus } from '../../reservations/domain/reservation';
import { ReservationOrmEntity } from '../../reservations/infrastructure/reservation.entity';
import { Concert, ConcertChanges, NewConcert } from '../domain/concert';
import { ConcertRepository, Page } from '../domain/concert.repository';
import { ConcertOrmEntity } from './concert.entity';

@Injectable()
export class TypeOrmConcertRepository implements ConcertRepository {
  constructor(private readonly dataSource: DataSource) {}

  private get concerts() {
    return currentManager(this.dataSource).getRepository(ConcertOrmEntity);
  }

  async create(concert: NewConcert): Promise<Concert> {
    return toDomain(await this.concerts.save(this.concerts.create(concert)));
  }

  async findById(id: string): Promise<Concert | null> {
    const row = await this.concerts.findOneBy({ id });
    return row && toDomain(row);
  }

  async findPage(page: number, limit: number): Promise<Page<Concert>> {
    const [rows, total] = await this.concerts.findAndCount({
      order: { startsAt: 'ASC', id: 'ASC' },
      skip: (page - 1) * limit,
      take: limit,
    });
    return { items: rows.map(toDomain), total };
  }

  async update(id: string, changes: ConcertChanges): Promise<Concert> {
    await this.concerts.update({ id }, changes);
    return toDomain(await this.concerts.findOneByOrFail({ id }));
  }

  async delete(id: string): Promise<void> {
    await this.concerts.delete({ id });
  }

  async findByIdForUpdate(id: string): Promise<Concert | null> {
    const row = await requireTransaction()
      .getRepository(ConcertOrmEntity)
      .findOne({ where: { id }, lock: { mode: 'pessimistic_write' } });
    return row && toDomain(row);
  }

  async reservedSeats(
    concertId: string,
    excludeReservationId?: string,
  ): Promise<number> {
    const qb = currentManager(this.dataSource)
      .getRepository(ReservationOrmEntity)
      .createQueryBuilder('r')
      .select('COALESCE(SUM(r.quantity), 0)', 'reserved')
      .where('r.concert_id = :concertId', { concertId })
      .andWhere('r.status = :status', { status: ReservationStatus.ACTIVE });
    if (excludeReservationId) {
      qb.andWhere('r.id <> :excludeReservationId', { excludeReservationId });
    }
    const result = await qb.getRawOne<{ reserved: string }>();
    return Number(result?.reserved ?? 0);
  }
}

function toDomain(row: ConcertOrmEntity): Concert {
  return {
    id: row.id,
    name: row.name,
    artist: row.artist,
    venue: row.venue,
    startsAt: row.startsAt,
    capacity: row.capacity,
    price: row.price,
    createdAt: row.createdAt,
  };
}
