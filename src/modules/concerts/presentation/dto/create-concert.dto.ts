import { Type } from 'class-transformer';
import {
  IsDate,
  IsInt,
  IsNotEmpty,
  IsString,
  MaxLength,
  Min,
} from 'class-validator';

export class CreateConcertDto {
  @IsString()
  @IsNotEmpty()
  @MaxLength(200)
  name: string;

  @IsString()
  @IsNotEmpty()
  @MaxLength(200)
  artist: string;

  @IsString()
  @IsNotEmpty()
  @MaxLength(200)
  venue: string;

  /** ISO 8601, e.g. 2026-12-31T19:00:00Z */
  @Type(() => Date)
  @IsDate()
  startsAt: Date;

  @IsInt()
  @Min(1)
  capacity: number;

  /** Whole IDR, no decimals. */
  @IsInt()
  @Min(0)
  price: number;
}
