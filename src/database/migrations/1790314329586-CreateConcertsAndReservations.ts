import { MigrationInterface, QueryRunner } from 'typeorm';

export class CreateConcertsAndReservations1790314329586 implements MigrationInterface {
  name = 'CreateConcertsAndReservations1790314329586';

  public async up(queryRunner: QueryRunner): Promise<void> {
    await queryRunner.query(
      `CREATE TABLE "concerts" ("id" uuid NOT NULL DEFAULT gen_random_uuid(), "name" character varying NOT NULL, "artist" character varying NOT NULL, "venue" character varying NOT NULL, "starts_at" TIMESTAMP WITH TIME ZONE NOT NULL, "capacity" integer NOT NULL, "price" integer NOT NULL, "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(), CONSTRAINT "CHK_concerts_price_non_negative" CHECK ("price" >= 0), CONSTRAINT "CHK_concerts_capacity_positive" CHECK ("capacity" > 0), CONSTRAINT "PK_6ca96059628588a3988a5f3236a" PRIMARY KEY ("id"))`,
    );
    await queryRunner.query(
      `CREATE TYPE "public"."reservation_status" AS ENUM('ACTIVE', 'CANCELLED')`,
    );
    await queryRunner.query(
      `CREATE TABLE "reservations" ("id" uuid NOT NULL DEFAULT gen_random_uuid(), "user_id" uuid NOT NULL, "concert_id" uuid NOT NULL, "quantity" integer NOT NULL, "status" "public"."reservation_status" NOT NULL DEFAULT 'ACTIVE', "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(), CONSTRAINT "CHK_reservations_quantity_positive" CHECK ("quantity" > 0), CONSTRAINT "PK_da95cef71b617ac35dc5bcda243" PRIMARY KEY ("id"))`,
    );
    await queryRunner.query(
      `CREATE INDEX "IDX_reservations_user" ON "reservations" ("user_id") `,
    );
    await queryRunner.query(
      `CREATE INDEX "IDX_reservations_concert_status" ON "reservations" ("concert_id", "status") `,
    );
    await queryRunner.query(
      `ALTER TABLE "reservations" ADD CONSTRAINT "FK_4af5055a871c46d011345a255a6" FOREIGN KEY ("user_id") REFERENCES "users"("id") ON DELETE CASCADE ON UPDATE NO ACTION`,
    );
    await queryRunner.query(
      `ALTER TABLE "reservations" ADD CONSTRAINT "FK_ff30cfc107bad916b328fa31c50" FOREIGN KEY ("concert_id") REFERENCES "concerts"("id") ON DELETE CASCADE ON UPDATE NO ACTION`,
    );
  }

  public async down(queryRunner: QueryRunner): Promise<void> {
    await queryRunner.query(
      `ALTER TABLE "reservations" DROP CONSTRAINT "FK_ff30cfc107bad916b328fa31c50"`,
    );
    await queryRunner.query(
      `ALTER TABLE "reservations" DROP CONSTRAINT "FK_4af5055a871c46d011345a255a6"`,
    );
    await queryRunner.query(
      `DROP INDEX "public"."IDX_reservations_concert_status"`,
    );
    await queryRunner.query(`DROP INDEX "public"."IDX_reservations_user"`);
    await queryRunner.query(`DROP TABLE "reservations"`);
    await queryRunner.query(`DROP TYPE "public"."reservation_status"`);
    await queryRunner.query(`DROP TABLE "concerts"`);
  }
}
