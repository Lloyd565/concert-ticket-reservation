import { Injectable, UnauthorizedException } from '@nestjs/common';
import { ConfigService } from '@nestjs/config';
import { PassportStrategy } from '@nestjs/passport';
import { ExtractJwt, Strategy } from 'passport-jwt';
import { EnvironmentVariables } from '../../../config/env.validation';
import { UserRepository } from '../../users/domain/user.repository';
import { AuthenticatedUser } from '../domain/authenticated-user';
import { JwtPayload } from '../domain/jwt-payload';

@Injectable()
export class JwtStrategy extends PassportStrategy(Strategy) {
  constructor(
    config: ConfigService<EnvironmentVariables, true>,
    private readonly users: UserRepository,
  ) {
    super({
      jwtFromRequest: ExtractJwt.fromAuthHeaderAsBearerToken(),
      secretOrKey: config.get('JWT_SECRET', { infer: true }),
      // Pinning the algorithm is what rejects `alg: none` and any attempt to
      // make us verify with a different algorithm than we sign with.
      algorithms: ['HS256'],
      ignoreExpiration: false,
    });
  }

  // Runs only after signature and expiry checks pass. Reloading the user means
  // a deleted account's still-valid token stops working immediately, and the
  // role comes from the database rather than from a possibly stale claim.
  async validate(payload: JwtPayload): Promise<AuthenticatedUser> {
    const user = await this.users.findById(payload.sub);
    if (!user) throw new UnauthorizedException();
    return { id: user.id, email: user.email, role: user.role };
  }
}
