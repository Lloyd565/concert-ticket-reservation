import { Role } from './role';
import { User } from './user';

export interface NewUser {
  email: string;
  passwordHash: string;
  role: Role;
}

/**
 * Persistence port for users. An abstract class (not an interface) so it can
 * double as the Nest DI token; infrastructure provides the implementation.
 */
export abstract class UserRepository {
  /** @throws EmailAlreadyRegisteredError if the email is taken. */
  abstract create(user: NewUser): Promise<User>;
  abstract findById(id: string): Promise<User | null>;
  abstract findByEmail(email: string): Promise<User | null>;
  abstract existsWithRole(role: Role): Promise<boolean>;
}
