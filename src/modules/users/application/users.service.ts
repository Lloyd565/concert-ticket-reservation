import { Injectable } from '@nestjs/common';
import { hash } from 'bcryptjs';
import { Role } from '../domain/role';
import { normalizeEmail, User } from '../domain/user';
import { UserRepository } from '../domain/user.repository';

const BCRYPT_COST = 10;

@Injectable()
export class UsersService {
  constructor(private readonly users: UserRepository) {}

  /** @throws EmailAlreadyRegisteredError */
  async createUser(email: string, password: string, role: Role): Promise<User> {
    return this.users.create({
      email: normalizeEmail(email),
      passwordHash: await hash(password, BCRYPT_COST),
      role,
    });
  }
}
