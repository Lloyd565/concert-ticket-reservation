export abstract class TransactionRunner {
  abstract run<T>(work: () => Promise<T>): Promise<T>;
}
