import { Role } from './role';
import { User } from './user';

export interface NewUser {
  email: string;
  passwordHash: string;
  role: Role;
}

export abstract class UserRepository {
  abstract create(user: NewUser): Promise<User>;
  abstract findById(id: string): Promise<User | null>;
  abstract findByEmail(email: string): Promise<User | null>;
  abstract existsWithRole(role: Role): Promise<boolean>;
}
