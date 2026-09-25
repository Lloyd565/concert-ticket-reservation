import { IsInt, Min } from 'class-validator';

export class UpdateReservationDto {
  @IsInt()
  @Min(1)
  quantity: number;
}
