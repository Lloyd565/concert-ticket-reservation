import { IsInt, IsUUID, Min } from 'class-validator';

export class CreateReservationDto {
  @IsUUID()
  concertId: string;

  @IsInt()
  @Min(1)
  quantity: number;
}
