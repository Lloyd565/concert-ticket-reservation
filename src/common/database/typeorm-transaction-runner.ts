import { AsyncLocalStorage } from 'async_hooks';
import { Injectable } from '@nestjs/common';
import { DataSource, EntityManager } from 'typeorm';
import { TransactionRunner } from '../application/transaction-runner';

const transactionContext = new AsyncLocalStorage<EntityManager>();

@Injectable()
export class TypeOrmTransactionRunner implements TransactionRunner {
  constructor(private readonly dataSource: DataSource) {}

  run<T>(work: () => Promise<T>): Promise<T> {
    if (transactionContext.getStore()) return work();
    return this.dataSource.transaction((manager) =>
      transactionContext.run(manager, work),
    );
  }
}

export function currentManager(dataSource: DataSource): EntityManager {
  return transactionContext.getStore() ?? dataSource.manager;
}

export function requireTransaction(): EntityManager {
  const manager = transactionContext.getStore();
  if (!manager) {
    throw new Error('Row locks must be taken inside TransactionRunner.run()');
  }
  return manager;
}
