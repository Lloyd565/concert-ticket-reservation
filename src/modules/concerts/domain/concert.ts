export interface Concert {
  id: string;
  name: string;
  artist: string;
  venue: string;
  startsAt: Date;
  capacity: number;
  /** Whole currency units (IDR has no minor unit); integer, never a float. */
  price: number;
  createdAt: Date;
}

export type NewConcert = Omit<Concert, 'id' | 'createdAt'>;
export type ConcertChanges = Partial<NewConcert>;
