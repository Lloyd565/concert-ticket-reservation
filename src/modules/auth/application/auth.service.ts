import { Injectable } from '@nestjs/common';
import { ConfigService } from '@nestjs/config';
import { JwtService } from '@nestjs/jwt';
import { compare, hashSync } from 'bcryptjs';
import {
  durationToSeconds,
  EnvironmentVariables,
} from '../../../config/env.validation';
import { UsersService } from '../../users/application/users.service';
import { Role } from '../../users/domain/role';
import { normalizeEmail, User } from '../../users/domain/user';
import { UserRepository } from '../../users/domain/user.repository';
import { InvalidCredentialsError } from '../domain/auth.errors';
import { JwtPayload } from '../domain/jwt-payload';

const DUMMY_HASH = hashSync('timing-equalizer', 10);

export interface AccessToken {
  accessToken: string;
  expiresIn: number;
}

@Injectable()
export class AuthService {
  constructor(
    private readonly users: UserRepository,
    private readonly usersService: UsersService,
    private readonly jwt: JwtService,
    private readonly config: ConfigService<EnvironmentVariables, true>,
  ) {}

  register(email: string, password: string): Promise<User> {
    return this.usersService.createUser(email, password, Role.USER);
  }

  async login(email: string, password: string): Promise<AccessToken> {
    const user = await this.users.findByEmail(normalizeEmail(email));
    const ok = await compare(password, user?.passwordHash ?? DUMMY_HASH);
    if (!user || !ok) throw new InvalidCredentialsError();

    const payload: JwtPayload = { sub: user.id, role: user.role };
    return {
      accessToken: await this.jwt.signAsync(payload),
      expiresIn: durationToSeconds(
        this.config.get('JWT_EXPIRES_IN', { infer: true }),
      ),
    };
  }
}
