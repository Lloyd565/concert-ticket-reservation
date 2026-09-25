import { TransactionRunner } from '../../../common/application/transaction-runner';
import { Concert } from '../../concerts/domain/concert';
import {
  ConcertNotFoundError,
  NotEnoughSeatsError,
} from '../../concerts/domain/concert.errors';
import { ConcertRepository } from '../../concerts/domain/concert.repository';
import { Role } from '../../users/domain/role';
import {
  NewReservation,
  Reservation,
  ReservationStatus,
} from '../domain/reservation';
import { ReservationNotFoundError } from '../domain/reservation.errors';
import { ReservationRepository } from '../domain/reservation.repository';
import { ReservationsService } from './reservations.service';

class InMemoryReservations extends ReservationRepository {
  rows: Reservation[] = [];

  create(r: NewReservation): Promise<Reservation> {
    const row = {
      ...r,
      id: `r${this.rows.length + 1}`,
      status: ReservationStatus.ACTIVE,
      createdAt: new Date(),
    };
    this.rows.push(row);
    return Promise.resolve(row);
  }
  findById(id: string) {
    return Promise.resolve(this.rows.find((r) => r.id === id) ?? null);
  }
  findAll(userId?: string) {
    return Promise.resolve(
      this.rows.filter((r) => !userId || r.userId === userId),
    );
  }
  save(r: Reservation) {
    this.rows = this.rows.map((row) => (row.id === r.id ? r : row));
    return Promise.resolve(r);
  }
}

class InMemoryConcerts extends ConcertRepository {
  constructor(
    private readonly concert: Concert,
    private readonly reservations: InMemoryReservations,
  ) {
    super();
  }
  findByIdForUpdate(id: string) {
    return Promise.resolve(id === this.concert.id ? this.concert : null);
  }
  reservedSeats(concertId: string, excludeId?: string) {
    return Promise.resolve(
      this.reservations.rows
        .filter(
          (r) =>
            r.concertId === concertId &&
            r.status === ReservationStatus.ACTIVE &&
            r.id !== excludeId,
        )
        .reduce((sum, r) => sum + r.quantity, 0),
    );
  }
  create = () => Promise.reject(new Error('unused'));
  findById = () => Promise.reject(new Error('unused'));
  findPage = () => Promise.reject(new Error('unused'));
  update = () => Promise.reject(new Error('unused'));
  delete = () => Promise.reject(new Error('unused'));
}

const inline: TransactionRunner = { run: (work) => work() };

describe('ReservationsService (fake repositories)', () => {
  const alice = { id: 'alice', email: 'a@x.com', role: Role.USER };
  const bob = { id: 'bob', email: 'b@x.com', role: Role.USER };
  let service: ReservationsService;

  beforeEach(() => {
    const reservations = new InMemoryReservations();
    const concert: Concert = {
      id: 'c1',
      name: 'n',
      artist: 'a',
      venue: 'v',
      startsAt: new Date(),
      capacity: 5,
      price: 0,
      createdAt: new Date(),
    };
    service = new ReservationsService(
      reservations,
      new InMemoryConcerts(concert, reservations),
      inline,
    );
  });

  it('rejects a reservation that would exceed capacity', async () => {
    await service.create(alice, 'c1', 4);
    await expect(service.create(bob, 'c1', 2)).rejects.toThrow(
      NotEnoughSeatsError,
    );
  });

  it('resizing excludes the reservation being resized from the count', async () => {
    const r = await service.create(alice, 'c1', 3);
    await expect(service.changeQuantity(alice, r.id, 5)).resolves.toMatchObject(
      { quantity: 5 },
    );
  });

  it("hides another user's reservation as not found", async () => {
    const r = await service.create(alice, 'c1', 1);
    await expect(service.get(bob, r.id)).rejects.toThrow(
      ReservationNotFoundError,
    );
  });

  it('reports an unknown concert', async () => {
    await expect(service.create(alice, 'nope', 1)).rejects.toThrow(
      ConcertNotFoundError,
    );
  });
});
