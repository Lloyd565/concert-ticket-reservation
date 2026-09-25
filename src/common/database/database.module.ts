import { Global, Module } from '@nestjs/common';
import { TransactionRunner } from '../application/transaction-runner';
import { TypeOrmTransactionRunner } from './typeorm-transaction-runner';

@Global()
@Module({
  providers: [
    { provide: TransactionRunner, useClass: TypeOrmTransactionRunner },
  ],
  exports: [TransactionRunner],
})
export class DatabaseModule {}
