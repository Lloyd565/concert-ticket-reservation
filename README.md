# Concert Ticket Reservation API

REST API untuk reservasi tiket konser, dibangun dengan **NestJS + TypeScript** dan **PostgreSQL**. Pengguna bisa mendaftar, login dengan JWT, melihat daftar konser, dan memesan kursi. Admin mengelola konser.

Aturan inti sistem: **untuk satu konser, jumlah kursi dari reservasi ACTIVE tidak pernah melebihi kapasitas, bahkan saat banyak request datang bersamaan.** Aturan ini dijaga dengan transaksi database dan row lock (`SELECT ... FOR UPDATE`), dan dibuktikan oleh test konkurensi terhadap PostgreSQL sungguhan.

## Pemenuhan Tugas

| Syarat | Dipenuhi oleh |
|---|---|
| a. Minimal 2 CRUD yang saling berelasi | `Concerts` dan `Reservations` (Reservation N:1 Concert, Reservation N:1 User). Lihat `src/modules/concerts/` dan `src/modules/reservations/` |
| b. Data di database SQL | PostgreSQL 17 via TypeORM, skema lewat migration di `src/database/migrations/` |
| c. Autentikasi API dengan JWT | `POST /auth/login` → JWT HS256. Guard global di `src/common/guards/` |
| d. E2E test untuk token/auth | `test/auth-token.e2e-spec.ts` (14 skenario wajib) + `test/reservations.e2e-spec.ts` |
| e. Pola proyek yang biasa saya pakai | Layered / Clean Architecture per modul. Lihat [Struktur Proyek](#struktur-proyek) |
| f. README menjelaskan alasan pola | Bagian [Mengapa Menggunakan Pola Ini?](#mengapa-menggunakan-pola-ini) |

## Tech Stack

- **Runtime:** Node.js 22, TypeScript (strict mode)
- **Framework:** NestJS 11
- **Database:** PostgreSQL 17, TypeORM 0.3 (migration, tanpa `synchronize`)
- **Auth:** `@nestjs/jwt`, `passport-jwt`, bcrypt (`bcryptjs`)
- **Validasi:** `class-validator`, `class-transformer`, validasi env saat boot
- **Dokumentasi API:** Swagger (`@nestjs/swagger`) di `/docs`
- **Testing:** Jest, Supertest
- **Kualitas kode:** ESLint, Prettier, GitHub Actions

## Prasyarat

- Node.js 22+ dan npm
- Docker (untuk PostgreSQL)

## Quick Start

```bash
# 1. Install dependency
npm install

# 2. Salin konfigurasi (isi JWT_SECRET dengan string acak ≥ 32 karakter)
cp .env.example .env

# 3. Jalankan PostgreSQL (dev DB "concert" dan test DB "concert_test")
docker compose up -d db

# 4. Buat skema
npm run migration:run

# 5. Jalankan API
npm run start:dev
```

API berjalan di `http://localhost:3000`, dan Swagger UI di `http://localhost:3000/docs`.

Saat pertama kali start, aplikasi membuat satu akun **ADMIN** dari `ADMIN_EMAIL` / `ADMIN_PASSWORD` (default `admin@example.com` / `admin12345`) kalau belum ada admin. Jadi aplikasi bisa langsung dicoba tanpa SQL manual.

> Port Postgres di host adalah **5433** (bukan 5432) supaya tidak bentrok dengan PostgreSQL yang terpasang langsung di mesin.

## Environment Variables

Semua variabel divalidasi saat boot (`src/config/env.validation.ts`). Kalau ada yang hilang atau salah, aplikasi langsung berhenti dengan pesan yang jelas.

| Variabel | Wajib | Default | Keterangan |
|---|---|---|---|
| `NODE_ENV` | tidak | `development` | `development` / `test` / `production` |
| `PORT` | tidak | `3000` | Port HTTP |
| `DB_HOST` | ya | | Host PostgreSQL |
| `DB_PORT` | ya | | Port PostgreSQL (docker compose: `5433`) |
| `DB_USERNAME` | ya | | User database |
| `DB_PASSWORD` | ya | | Password database |
| `DB_NAME` | ya | | Nama database (`concert`) |
| `JWT_SECRET` | ya | | Kunci HMAC untuk HS256, **minimal 32 karakter** |
| `JWT_EXPIRES_IN` | tidak | `15m` | Masa berlaku token: `<angka><s\|m\|h\|d>` |
| `ADMIN_EMAIL` | ya | | Email admin awal |
| `ADMIN_PASSWORD` | ya | | Password admin awal (minimal 8 karakter) |

## Endpoint

| Method | Path | Auth | Role | Keterangan |
|---|---|---|---|---|
| POST | `/auth/register` | tidak | | Daftar akun (selalu role `USER`). 201, email duplikat 409 |
| POST | `/auth/login` | tidak | | `{ accessToken, expiresIn }`. Gagal → 401 yang sama untuk email maupun password salah |
| GET | `/auth/me` | ya | apa saja | `{ id, email, role }` |
| GET | `/concerts?page=&limit=` | tidak | | Daftar konser dengan paginasi (`limit` maks. 100) |
| GET | `/concerts/:id` | tidak | | Detail konser |
| POST | `/concerts` | ya | ADMIN | Buat konser |
| PATCH | `/concerts/:id` | ya | ADMIN | Ubah konser. Kapasitas tidak boleh di bawah kursi yang sudah dipesan (409) |
| DELETE | `/concerts/:id` | ya | ADMIN | Hapus konser. 409 kalau masih ada reservasi ACTIVE |
| POST | `/reservations` | ya | apa saja | `{ concertId, quantity }`. 409 kalau kursi tidak cukup |
| GET | `/reservations` | ya | apa saja | Milik sendiri. ADMIN melihat semua |
| GET | `/reservations/:id` | ya | apa saja | Reservasi milik orang lain → 404 |
| PATCH | `/reservations/:id` | ya | apa saja | Ubah `quantity` (hanya ACTIVE, kapasitas dicek ulang) |
| DELETE | `/reservations/:id` | ya | apa saja | Batalkan (status `CANCELLED`, kursi dibebaskan) |

Harga (`price`) disimpan sebagai **integer rupiah**, bukan float.

## Alur Autentikasi

1. Klien `POST /auth/login`. Server mencocokkan password dengan hash bcrypt, lalu menandatangani JWT **HS256** berisi `sub` (id user), `role`, `iat`, dan `exp`.
2. Klien mengirim `Authorization: Bearer <token>` di setiap request.
3. **`JwtAuthGuard`** dipasang **global**, jadi semua route terlindungi kecuali yang diberi `@Public()` (*secure by default*).
4. **`JwtStrategy`** memverifikasi signature dan `exp`. Algoritma dipatok ke `HS256`, sehingga token `alg: none` ditolak. Setelah itu strategy **memuat ulang user dari DB** berdasarkan `sub`, jadi token milik user yang sudah dihapus langsung ditolak, dan role dibaca dari DB, bukan dari token. Hasilnya dipasang ke `request.user`.
5. **`RolesGuard`** mencocokkan `request.user.role` dengan `@Roles(Role.ADMIN)`. Kalau tidak cocok, responsnya 403.
6. Controller menerima user lewat `@CurrentUser()`.

Contoh:

```bash
curl -X POST localhost:3000/auth/register -H "Content-Type: application/json" \
  -d '{"email":"budi@example.com","password":"rahasia123"}'

TOKEN=$(curl -s -X POST localhost:3000/auth/login -H "Content-Type: application/json" \
  -d '{"email":"budi@example.com","password":"rahasia123"}' | node -pe 'JSON.parse(require("fs").readFileSync(0)).accessToken')

curl localhost:3000/auth/me -H "Authorization: Bearer $TOKEN"
```

## Menjalankan Test

```bash
npm test            # unit test (tanpa database)
npm run test:e2e    # e2e test (butuh: docker compose up -d db)
npm run lint
npm run build
```

- **Unit test** (`src/**/*.spec.ts`): aturan kapasitas di domain, `ReservationsService` dengan *fake repository*, pemetaan error, dan validasi env.
- **E2E test** (`test/*.e2e-spec.ts`): berjalan terhadap **PostgreSQL sungguhan** di database `concert_test` (konfigurasi di `.env.test`, yang sengaja di-commit karena isinya kredensial dummy khusus test).
  - Skema dibuat dari migration yang sama dengan aplikasi.
  - Semua tabel dikosongkan sebelum setiap test.
  - Test **menolak berjalan** kalau nama database tidak berakhiran `_test`, supaya database dev tidak mungkin terhapus.
  - Salah satunya adalah **test konkurensi**: 40 reservasi paralel ke konser berkapasitas 3, lalu jumlah kursi ACTIVE dicek **langsung ke database**. Test ini sudah diverifikasi **gagal** kalau `FOR UPDATE` dihapus, jadi test ini benar-benar membuktikan lock-nya.

## Struktur Proyek

```
src/
├── main.ts                     # bootstrap + Swagger
├── app.module.ts               # wiring modul, pipe/filter/guard global
├── config/                     # validasi env, opsi TypeORM
├── database/
│   ├── data-source.ts          # entry point CLI migration
│   └── migrations/
├── common/
│   ├── application/            # port TransactionRunner
│   ├── database/               # implementasi TypeORM (AsyncLocalStorage)
│   ├── decorators/             # @Public, @Roles, @CurrentUser
│   ├── domain/                 # DomainError (base class)
│   ├── filters/                # DomainError → status HTTP (satu tempat)
│   └── guards/                 # JwtAuthGuard, RolesGuard
└── modules/
    ├── auth/  users/  concerts/  reservations/
    │   ├── presentation/       # controller + DTO (class-validator)
    │   ├── application/        # service / use case, batas transaksi
    │   ├── domain/             # model, aturan bisnis, error, port repository
    │   └── infrastructure/     # entity TypeORM, implementasi repository
test/
├── auth-token.e2e-spec.ts
├── reservations.e2e-spec.ts
├── global-setup.ts             # migration ke concert_test
├── test-env.ts                 # pengaman: hanya DB *_test
└── utils/test-app.ts
```

## Mengapa Menggunakan Pola Ini?

### Polanya

Proyek ini memakai **layered architecture ala Clean Architecture per modul fitur**. Setiap modul di `src/modules/<fitur>/` dibagi menjadi empat lapisan, dan **dependensi hanya boleh mengarah ke dalam**:

```
presentation  →  application  →  domain  ←  infrastructure
```

- **`domain/`** adalah inti: model, aturan bisnis, error bisnis, dan *port* repository. Contohnya `src/modules/concerts/domain/capacity.ts` (aturan "kursi tidak boleh melebihi kapasitas") dan `concert.repository.ts` (abstract class `ConcertRepository`). Lapisan ini TypeScript murni, **tanpa** NestJS maupun TypeORM.
- **`application/`** berisi use case, misalnya `reservations.service.ts`. Di lapisan inilah batas transaksi ditentukan (`TransactionRunner.run(...)`). Service hanya mengenal port dari domain, bukan TypeORM.
- **`infrastructure/`** berisi detail teknis: entity TypeORM dan `TypeOrmConcertRepository`, yang **mengimplementasikan** port domain. Di sinilah `SELECT ... FOR UPDATE` berada.
- **`presentation/`** berisi controller dan DTO. Controller hanya menerima request, memvalidasi lewat DTO, memanggil service, dan tidak berisi logika bisnis.

Implementasi disambungkan ke port lewat DI NestJS, misalnya di `concerts.module.ts`:

```ts
{ provide: ConcertRepository, useClass: TypeOrmConcertRepository }
```

Aturan arah dependensi ini **ditegakkan otomatis** oleh ESLint (`no-restricted-imports` di `eslint.config.mjs`). Kalau `domain/` mengimpor `@nestjs/*` atau `typeorm`, atau service mengimpor `typeorm`, `npm run lint` gagal.

### Alasannya

- **Separation of concerns.** Aturan bisnis yang paling penting, yaitu kapasitas konser, ada di satu fungsi murni (`capacity.ts`) dan tidak tersebar di controller atau query SQL. Pemetaan error bisnis ke status HTTP juga hanya ada di satu tempat (`domain-exception.filter.ts`).
- **Testability.** Karena service bergantung pada port (abstract class), service bisa diuji dengan *fake repository* in-memory tanpa database. Contohnya ada di `src/modules/reservations/application/reservations.service.spec.ts`. Aturan domain diuji sebagai fungsi biasa (`capacity.spec.ts`).
- **Persistence bisa diganti.** Service tidak tahu apakah data disimpan dengan TypeORM, Prisma, atau SQL mentah. Mengganti ORM cukup dengan menulis implementasi baru dari port yang sama, tanpa menyentuh service atau controller.
- **Cocok dengan DI NestJS.** Abstract class bisa langsung dipakai sebagai token DI, jadi pola port/adapter ini tidak butuh library tambahan dan tetap terasa "Nest-idiomatic".
- **Aturan bisnis bebas dari framework dan database.** Karena dependensi mengarah ke dalam, perubahan framework, ORM, atau format HTTP tidak memaksa perubahan pada `domain/`.

### Kenapa ini pola default saya

Pola layering dengan dependensi ke arah dalam ini **sama dengan yang saya pakai di proyek sebelumnya**: versi awal repo ini adalah sistem reservasi tiket konser berbasis **Go microservices** (masih tersimpan di tag git `go-microservices-p4`, dan dokumen desainnya ada di `docs/archive/`). Di sana setiap service memakai susunan `domain ← usecase ← repository/transport`, dengan aturan yang sama: domain tidak mengimpor apa pun dari luar, interface repository dideklarasikan di lapisan usecase, dan transaksi dibuka di usecase.

Di proyek ini polanya saya adaptasi ke NestJS (`usecase` menjadi `application`, `transport` menjadi `presentation`). Karena sudah terbukti membuat kode yang kompleks, terutama bagian locking dan konkurensi, tetap mudah dipahami dan diuji, pola ini jadi pilihan default saya.

### Trade-off yang jujur

- **Boilerplate lebih banyak.** Untuk aplikasi sekecil ini ada port, implementasi, dan fungsi mapper `toDomain()` yang terasa berulang. Satu fitur bisa menyentuh 4–6 file.
- **Transaksi implisit.** `TransactionRunner` menyimpan transaksi di `AsyncLocalStorage` supaya service tidak menyentuh TypeORM, tapi akibatnya keikutsertaan repository dalam transaksi tidak terlihat di signature method. Untuk menutup celah ini, method yang mengambil lock (`findByIdForUpdate`) **menolak berjalan** di luar transaksi.

Pola ini tetap sepadan karena bagian tersulit sistem (invariant kapasitas di bawah konkurensi) jadi terisolasi dengan jelas. Lock ada di repository, batas transaksi di service, aturannya di domain, dan masing-masing bisa diuji terpisah.

### Yang sengaja tidak dipakai

- **Microservices, gRPC, message broker, Redis.** Versi Go sebelumnya memakai semua ini untuk mengeksplorasi sistem terdistribusi. Untuk tugas ini satu aplikasi monolit dengan satu database sudah cukup, dan justru lebih mudah menjamin konsistensi: satu transaksi PostgreSQL menggantikan saga dan outbox.
- **CQRS dan event sourcing.** Tidak ada kebutuhan baca/tulis terpisah atau riwayat event, jadi pola ini hanya akan menambah kompleksitas.
- **Refresh token.** Di luar lingkup tugas. Access token dibuat berumur pendek (15 menit).

## Lisensi

UNLICENSED, dibuat untuk keperluan tugas kuliah.
