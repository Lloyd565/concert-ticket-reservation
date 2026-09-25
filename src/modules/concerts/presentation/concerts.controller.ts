import {
  Body,
  Controller,
  Delete,
  Get,
  HttpCode,
  HttpStatus,
  Param,
  ParseUUIDPipe,
  Patch,
  Post,
  Query,
} from '@nestjs/common';
import { ApiBearerAuth, ApiTags } from '@nestjs/swagger';
import { Public } from '../../../common/decorators/public.decorator';
import { Roles } from '../../../common/decorators/roles.decorator';
import { Role } from '../../users/domain/role';
import { ConcertsService } from '../application/concerts.service';
import { CreateConcertDto } from './dto/create-concert.dto';
import { ListConcertsQuery } from './dto/list-concerts.query';
import { UpdateConcertDto } from './dto/update-concert.dto';

@ApiTags('concerts')
@Controller('concerts')
export class ConcertsController {
  constructor(private readonly concerts: ConcertsService) {}

  @Public()
  @Get()
  async list(@Query() { page, limit }: ListConcertsQuery) {
    const { items, total } = await this.concerts.list(page, limit);
    return { items, page, limit, total };
  }

  @Public()
  @Get(':id')
  get(@Param('id', ParseUUIDPipe) id: string) {
    return this.concerts.get(id);
  }

  @ApiBearerAuth()
  @Roles(Role.ADMIN)
  @Post()
  create(@Body() dto: CreateConcertDto) {
    return this.concerts.create(dto);
  }

  @ApiBearerAuth()
  @Roles(Role.ADMIN)
  @Patch(':id')
  update(
    @Param('id', ParseUUIDPipe) id: string,
    @Body() dto: UpdateConcertDto,
  ) {
    return this.concerts.update(id, dto);
  }

  @ApiBearerAuth()
  @Roles(Role.ADMIN)
  @Delete(':id')
  @HttpCode(HttpStatus.NO_CONTENT)
  delete(@Param('id', ParseUUIDPipe) id: string) {
    return this.concerts.delete(id);
  }
}
