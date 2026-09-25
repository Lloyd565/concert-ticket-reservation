import { Role } from './role';

export interface User {
  id: string;
  email: string;
  passwordHash: string;
  role: Role;
  createdAt: Date;
}

export function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}
