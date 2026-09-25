export interface Concert {
  id: string;
  name: string;
  artist: string;
  venue: string;
  startsAt: Date;
  capacity: number;
  price: number;
  createdAt: Date;
}

export type NewConcert = Omit<Concert, 'id' | 'createdAt'>;
export type ConcertChanges = Partial<NewConcert>;
