import { Injectable } from '@nestjs/common';
import { DataSource } from 'typeorm';
import { currentManager } from '../../../common/database/typeorm-transaction-runner';
import { NewReservation, Reservation } from '../domain/reservation';
import { ReservationRepository } from '../domain/reservation.repository';
import { ReservationOrmEntity } from './reservation.entity';

@Injectable()
export class TypeOrmReservationRepository implements ReservationRepository {
  constructor(private readonly dataSource: DataSource) {}

  private get reservations() {
    return currentManager(this.dataSource).getRepository(ReservationOrmEntity);
  }

  async create(reservation: NewReservation): Promise<Reservation> {
    return toDomain(
      await this.reservations.save(this.reservations.create(reservation)),
    );
  }

  async findById(id: string): Promise<Reservation | null> {
    const row = await this.reservations.findOneBy({ id });
    return row && toDomain(row);
  }

  async findAll(userId?: string): Promise<Reservation[]> {
    const rows = await this.reservations.find({
      where: userId ? { userId } : {},
      order: { createdAt: 'DESC' },
    });
    return rows.map(toDomain);
  }

  async save(reservation: Reservation): Promise<Reservation> {
    await this.reservations.update(
      { id: reservation.id },
      { quantity: reservation.quantity, status: reservation.status },
    );
    return reservation;
  }
}

function toDomain(row: ReservationOrmEntity): Reservation {
  return {
    id: row.id,
    userId: row.userId,
    concertId: row.concertId,
    quantity: row.quantity,
    status: row.status,
    createdAt: row.createdAt,
  };
}
