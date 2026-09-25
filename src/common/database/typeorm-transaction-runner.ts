import { AsyncLocalStorage } from 'async_hooks';
import { Injectable } from '@nestjs/common';
import { DataSource, EntityManager } from 'typeorm';
import { TransactionRunner } from '../application/transaction-runner';

// Holds the transaction's EntityManager for the duration of `run`, so
// repositories can join it without services passing a handle around.
const transactionContext = new AsyncLocalStorage<EntityManager>();

@Injectable()
export class TypeOrmTransactionRunner implements TransactionRunner {
  constructor(private readonly dataSource: DataSource) {}

  run<T>(work: () => Promise<T>): Promise<T> {
    // Nested run joins the outer transaction; opening a second one would use
    // another connection and could not see (or would block on) the outer locks.
    if (transactionContext.getStore()) return work();
    return this.dataSource.transaction((manager) =>
      transactionContext.run(manager, work),
    );
  }
}

/** The active transaction's manager, or the default one outside `run`. */
export function currentManager(dataSource: DataSource): EntityManager {
  return transactionContext.getStore() ?? dataSource.manager;
}

/**
 * For row locks: a FOR UPDATE outside a transaction is released as soon as the
 * statement ends and protects nothing, so refuse instead of silently no-op'ing.
 */
export function requireTransaction(): EntityManager {
  const manager = transactionContext.getStore();
  if (!manager) {
    throw new Error('Row locks must be taken inside TransactionRunner.run()');
  }
  return manager;
}
