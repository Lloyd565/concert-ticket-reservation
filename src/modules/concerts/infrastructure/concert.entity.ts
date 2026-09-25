import {
  Check,
  Column,
  CreateDateColumn,
  Entity,
  PrimaryGeneratedColumn,
} from 'typeorm';

@Entity('concerts')
@Check('CHK_concerts_capacity_positive', `"capacity" > 0`)
@Check('CHK_concerts_price_non_negative', `"price" >= 0`)
export class ConcertOrmEntity {
  @PrimaryGeneratedColumn('uuid')
  id: string;

  @Column()
  name: string;

  @Column()
  artist: string;

  @Column()
  venue: string;

  @Column({ name: 'starts_at', type: 'timestamptz' })
  startsAt: Date;

  @Column({ type: 'int' })
  capacity: number;

  @Column({ type: 'int' })
  price: number;

  @CreateDateColumn({ name: 'created_at', type: 'timestamptz' })
  createdAt: Date;
}
