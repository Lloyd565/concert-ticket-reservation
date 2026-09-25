import { DomainError } from '../../../common/domain/domain-error';

// One error for both "no such email" and "wrong password", so the response
// cannot be used to discover which emails are registered.
export class InvalidCredentialsError extends DomainError {
  readonly kind = 'UNAUTHORIZED';

  constructor() {
    super('Invalid email or password');
  }
}
