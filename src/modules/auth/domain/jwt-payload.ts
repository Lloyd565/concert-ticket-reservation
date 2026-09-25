import { Role } from '../../users/domain/role';

/** Claims we sign; `iat` and `exp` are added by the JWT library. */
export interface JwtPayload {
  sub: string;
  role: Role;
}
