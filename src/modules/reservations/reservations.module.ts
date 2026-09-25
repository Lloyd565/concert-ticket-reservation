import { Module } from '@nestjs/common';
import { ConcertsModule } from '../concerts/concerts.module';
import { ReservationsService } from './application/reservations.service';
import { ReservationRepository } from './domain/reservation.repository';
import { TypeOrmReservationRepository } from './infrastructure/typeorm-reservation.repository';
import { ReservationsController } from './presentation/reservations.controller';

@Module({
  imports: [ConcertsModule],
  controllers: [ReservationsController],
  providers: [
    ReservationsService,
    { provide: ReservationRepository, useClass: TypeOrmReservationRepository },
  ],
})
export class ReservationsModule {}
