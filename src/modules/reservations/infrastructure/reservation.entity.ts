import {
  Check,
  Column,
  CreateDateColumn,
  Entity,
  Index,
  JoinColumn,
  ManyToOne,
  PrimaryGeneratedColumn,
} from 'typeorm';
import { ConcertOrmEntity } from '../../concerts/infrastructure/concert.entity';
import { UserOrmEntity } from '../../users/infrastructure/user.entity';
import { ReservationStatus } from '../domain/reservation';

@Entity('reservations')
@Check('CHK_reservations_quantity_positive', `"quantity" > 0`)
@Index('IDX_reservations_concert_status', ['concertId', 'status'])
@Index('IDX_reservations_user', ['userId'])
export class ReservationOrmEntity {
  @PrimaryGeneratedColumn('uuid')
  id: string;

  @Column({ name: 'user_id', type: 'uuid' })
  userId: string;

  @ManyToOne(() => UserOrmEntity, { onDelete: 'CASCADE' })
  @JoinColumn({ name: 'user_id' })
  user?: UserOrmEntity;

  @Column({ name: 'concert_id', type: 'uuid' })
  concertId: string;

  @ManyToOne(() => ConcertOrmEntity, { onDelete: 'CASCADE' })
  @JoinColumn({ name: 'concert_id' })
  concert?: ConcertOrmEntity;

  @Column({ type: 'int' })
  quantity: number;

  @Column({
    type: 'enum',
    enum: ReservationStatus,
    enumName: 'reservation_status',
    default: ReservationStatus.ACTIVE,
  })
  status: ReservationStatus;

  @CreateDateColumn({ name: 'created_at', type: 'timestamptz' })
  createdAt: Date;
}
