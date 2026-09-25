import { Injectable } from '@nestjs/common';
import { TransactionRunner } from '../../../common/application/transaction-runner';
import { AuthenticatedUser } from '../../auth/domain/authenticated-user';
import { assertSeatsAvailable } from '../../concerts/domain/capacity';
import { ConcertNotFoundError } from '../../concerts/domain/concert.errors';
import { ConcertRepository } from '../../concerts/domain/concert.repository';
import { Role } from '../../users/domain/role';
import { Reservation, ReservationStatus } from '../domain/reservation';
import {
  ReservationNotActiveError,
  ReservationNotFoundError,
} from '../domain/reservation.errors';
import { ReservationRepository } from '../domain/reservation.repository';

@Injectable()
export class ReservationsService {
  constructor(
    private readonly reservations: ReservationRepository,
    private readonly concerts: ConcertRepository,
    private readonly tx: TransactionRunner,
  ) {}

  create(
    user: AuthenticatedUser,
    concertId: string,
    quantity: number,
  ): Promise<Reservation> {
    return this.tx.run(async () => {
      const concert = await this.concerts.findByIdForUpdate(concertId);
      if (!concert) throw new ConcertNotFoundError();
      assertSeatsAvailable(
        concert.capacity,
        await this.concerts.reservedSeats(concertId),
        quantity,
      );
      return this.reservations.create({ userId: user.id, concertId, quantity });
    });
  }

  list(user: AuthenticatedUser): Promise<Reservation[]> {
    return this.reservations.findAll(
      user.role === Role.ADMIN ? undefined : user.id,
    );
  }

  async get(user: AuthenticatedUser, id: string): Promise<Reservation> {
    const reservation = await this.reservations.findById(id);
    if (!reservation || !canSee(user, reservation)) {
      throw new ReservationNotFoundError();
    }
    return reservation;
  }

  async changeQuantity(
    user: AuthenticatedUser,
    id: string,
    quantity: number,
  ): Promise<Reservation> {
    const { concertId } = await this.get(user, id);
    return this.tx.run(async () => {
      const concert = await this.concerts.findByIdForUpdate(concertId);
      if (!concert) throw new ConcertNotFoundError();
      const reservation = await this.reloadActive(id);
      assertSeatsAvailable(
        concert.capacity,
        await this.concerts.reservedSeats(concertId, id),
        quantity,
      );
      return this.reservations.save({ ...reservation, quantity });
    });
  }

  async cancel(user: AuthenticatedUser, id: string): Promise<Reservation> {
    const { concertId } = await this.get(user, id);
    return this.tx.run(async () => {
      await this.concerts.findByIdForUpdate(concertId);
      const reservation = await this.reloadActive(id);
      return this.reservations.save({
        ...reservation,
        status: ReservationStatus.CANCELLED,
      });
    });
  }

  private async reloadActive(id: string): Promise<Reservation> {
    const reservation = await this.reservations.findById(id);
    if (!reservation) throw new ReservationNotFoundError();
    if (reservation.status !== ReservationStatus.ACTIVE) {
      throw new ReservationNotActiveError();
    }
    return reservation;
  }
}

function canSee(user: AuthenticatedUser, reservation: Reservation): boolean {
  return user.role === Role.ADMIN || reservation.userId === user.id;
}
