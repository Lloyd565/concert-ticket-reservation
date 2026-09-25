/**
 * Port for running a unit of work atomically. Services depend on this, not on
 * TypeORM; repositories called inside `run` automatically join the transaction.
 */
export abstract class TransactionRunner {
  abstract run<T>(work: () => Promise<T>): Promise<T>;
}
