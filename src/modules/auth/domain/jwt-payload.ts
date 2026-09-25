import { Role } from '../../users/domain/role';

export interface JwtPayload {
  sub: string;
  role: Role;
}
