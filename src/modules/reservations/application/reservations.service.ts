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

// Every write below locks the concert row first, then reads seat counts.
// One lock, always taken first, in every path: that is what makes the
// capacity check race-free and rules out lock-order deadlocks.
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
      // Exclude this reservation's current seats: it is being resized, not
      // added on top of itself.
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
      // Same lock as every other seat change, so a cancel cannot interleave
      // with a concurrent resize of this reservation.
      await this.concerts.findByIdForUpdate(concertId);
      const reservation = await this.reloadActive(id);
      return this.reservations.save({
        ...reservation,
        status: ReservationStatus.CANCELLED,
      });
    });
  }

  // Re-read after taking the lock: what we saw before it may be stale.
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
