import { DomainError } from '../../../common/domain/domain-error';

export class InvalidCredentialsError extends DomainError {
  readonly kind = 'UNAUTHORIZED';

  constructor() {
    super('Invalid email or password');
  }
}
