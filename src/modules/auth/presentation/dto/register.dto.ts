import { IsEmail, IsString, MaxLength, MinLength } from 'class-validator';

export class RegisterDto {
  @IsEmail()
  @MaxLength(254)
  email: string;

  // bcrypt ignores everything past 72 bytes, so longer passwords would give a
  // false sense of strength.
  @IsString()
  @MinLength(8)
  @MaxLength(72)
  password: string;
}
