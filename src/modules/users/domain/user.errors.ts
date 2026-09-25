import { DomainError } from '../../../common/domain/domain-error';

export class EmailAlreadyRegisteredError extends DomainError {
  readonly kind = 'CONFLICT';

  constructor() {
    super('Email is already registered');
  }
}
