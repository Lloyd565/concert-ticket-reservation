export enum ReservationStatus {
  ACTIVE = 'ACTIVE',
  CANCELLED = 'CANCELLED',
}

export interface Reservation {
  id: string;
  userId: string;
  concertId: string;
  quantity: number;
  status: ReservationStatus;
  createdAt: Date;
}

export type NewReservation = Pick<
  Reservation,
  'userId' | 'concertId' | 'quantity'
>;
