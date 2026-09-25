import { Module } from '@nestjs/common';
import { ConcertsService } from './application/concerts.service';
import { ConcertRepository } from './domain/concert.repository';
import { TypeOrmConcertRepository } from './infrastructure/typeorm-concert.repository';
import { ConcertsController } from './presentation/concerts.controller';

@Module({
  controllers: [ConcertsController],
  providers: [
    ConcertsService,
    { provide: ConcertRepository, useClass: TypeOrmConcertRepository },
  ],
  exports: [ConcertRepository],
})
export class ConcertsModule {}
