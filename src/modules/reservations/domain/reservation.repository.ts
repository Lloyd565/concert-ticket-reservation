import { NewReservation, Reservation } from './reservation';

export abstract class ReservationRepository {
  abstract create(reservation: NewReservation): Promise<Reservation>;
  abstract findById(id: string): Promise<Reservation | null>;
  /** All reservations, or only one user's when `userId` is given. */
  abstract findAll(userId?: string): Promise<Reservation[]>;
  abstract save(reservation: Reservation): Promise<Reservation>;
}
