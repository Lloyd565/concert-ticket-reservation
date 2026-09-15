//go:build integration

// Integration tests for the outbox relay: a real Postgres holding the outbox and
// a real RabbitMQ receiving it. A fake broker would confirm whatever the relay
// hoped it would, and publisher confirms are exactly the part under test.
package events_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/events"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/repository/postgres"
)

var (
	testPool *pgxpool.Pool
	amqpURL  string
)

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	ctx := context.Background()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("booking_db"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(pg) }()

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}
	if testPool, err = pgxpool.New(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "connect pool: %v\n", err)
		return 1
	}
	defer testPool.Close()
	if err := applyMigrations(ctx, testPool); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		return 1
	}

	rabbit, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "rabbitmq:4-management-alpine",
			ExposedPorts: []string{"5672/tcp"},
			WaitingFor:   wait.ForLog("Server startup complete").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start rabbitmq container: %v\n", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(rabbit) }()
	host, err := rabbit.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rabbitmq host: %v\n", err)
		return 1
	}
	port, err := rabbit.MappedPort(ctx, "5672/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "rabbitmq port: %v\n", err)
		return 1
	}
	amqpURL = fmt.Sprintf("amqp://guest:guest@%s:%s/", host, port.Port())

	return m.Run()
}

// applyMigrations runs the checked-in .up.sql files: the outbox under test is the
// outbox that ships.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found")
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// enqueue commits events for a fresh aggregate through the real write path - in
// a transaction, as every use case does - and returns the aggregate's ID.
func enqueue(t *testing.T, ctx context.Context, eventTypes ...string) string {
	t.Helper()
	repo := postgres.NewRepo(testPool)
	aggregateID := uuid.Must(uuid.NewV7()).String()
	err := postgres.NewTxManager(testPool).WithinTx(ctx, func(ctx context.Context) error {
		for i, eventType := range eventTypes {
			if err := repo.Enqueue(ctx, eventType, aggregateID, map[string]int{"seq": i}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return aggregateID
}

type outboxRow struct {
	ID        string
	EventType string
	Published bool
	Leased    bool
}

func outboxRows(t *testing.T, ctx context.Context, aggregateID string) []outboxRow {
	t.Helper()
	rows, err := testPool.Query(ctx, `
		SELECT id::text, event_type, published_at IS NOT NULL, claimed_until IS NOT NULL
		FROM outbox WHERE aggregate_id = $1 ORDER BY created_at`, aggregateID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.EventType, &r.Published, &r.Leased); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// relayAll runs a relay until it finds nothing left to publish.
func relayAll(t *testing.T, ctx context.Context, relay *events.Relay) {
	t.Helper()
	for range 50 {
		n, err := relay.RelayOnce(ctx)
		if err != nil {
			t.Fatalf("relay: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("the relay never drained the outbox")
}

// subscribe binds a private queue to every event on the exchange, standing in
// for a consumer. It must exist before the publish: an unbound topic exchange
// drops what it cannot route.
func subscribe(t *testing.T) (*amqp.Channel, string) {
	t.Helper()
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	if err := ch.ExchangeDeclare(events.Exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		t.Fatalf("declare exchange: %v", err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatalf("declare queue: %v", err)
	}
	if err := ch.QueueBind(q.Name, "#", events.Exchange, false, nil); err != nil {
		t.Fatalf("bind queue: %v", err)
	}
	return ch, q.Name
}

// receive waits for the messages with the given IDs, ignoring any others.
func receive(t *testing.T, ch *amqp.Channel, queue string, ids ...string) map[string]amqp.Delivery {
	t.Helper()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	got := make(map[string]amqp.Delivery, len(ids))
	for deadline := time.Now().Add(10 * time.Second); len(got) < len(want) && time.Now().Before(deadline); {
		d, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if want[d.MessageId] {
			got[d.MessageId] = d
		}
	}
	if len(got) != len(want) {
		t.Fatalf("received %d of %d published events", len(got), len(want))
	}
	return got
}

func TestRelayPublishesCommittedEventsAndMarksThemSent(t *testing.T) {
	ctx := context.Background()
	ch, queue := subscribe(t)
	aggregateID := enqueue(t, ctx, domain.EventReservationHeld, domain.EventReservationExpired)

	relayAll(t, ctx, events.NewRelay(amqpURL, postgres.NewRepo(testPool), quietLog()))

	rows := outboxRows(t, ctx, aggregateID)
	if len(rows) != 2 {
		t.Fatalf("want 2 outbox rows, got %d", len(rows))
	}
	got := receive(t, ch, queue, rows[0].ID, rows[1].ID)
	for _, row := range rows {
		if !row.Published {
			t.Fatalf("event %s was delivered but never marked published, so it would be sent forever", row.ID)
		}
		d := got[row.ID]
		if d.RoutingKey != row.EventType {
			t.Fatalf("want routing key %s, got %s", row.EventType, d.RoutingKey)
		}
		if d.DeliveryMode != amqp.Persistent {
			t.Fatalf("event %s published transient: a broker restart would drop it", row.ID)
		}
		var env struct {
			EventID   string `json:"event_id"`
			EventType string `json:"event_type"`
			Version   int    `json:"version"`
		}
		if err := json.Unmarshal(d.Body, &env); err != nil {
			t.Fatalf("decode published envelope: %v", err)
		}
		if env.EventID != row.ID || env.EventType != row.EventType || env.Version != domain.EventVersion {
			t.Fatalf("published envelope does not match its outbox row: %+v vs %+v", env, row)
		}
	}
}

// TestRelayKeepsEventsWhileTheBrokerIsUnreachable is §3.3's "event bus down"
// row: nothing is lost and nothing is held back, and the event goes out once
// the broker is reachable again.
func TestRelayKeepsEventsWhileTheBrokerIsUnreachable(t *testing.T) {
	ctx := context.Background()
	aggregateID := enqueue(t, ctx, domain.EventBookingConfirmed)

	down := events.NewRelay("amqp://guest:guest@127.0.0.1:1/", postgres.NewRepo(testPool), quietLog())
	if _, err := down.RelayOnce(ctx); err == nil {
		t.Fatal("a relay that cannot reach the broker must report it")
	}
	row := outboxRows(t, ctx, aggregateID)[0]
	if row.Published || row.Leased {
		t.Fatalf("a relay that cannot reach the broker must neither publish nor lease events: %+v", row)
	}

	ch, queue := subscribe(t)
	relayAll(t, ctx, events.NewRelay(amqpURL, postgres.NewRepo(testPool), quietLog()))
	receive(t, ch, queue, row.ID)
	if !outboxRows(t, ctx, aggregateID)[0].Published {
		t.Fatal("the event that waited out the outage was delivered but not marked published")
	}
}

// TestRelaySkipsAnEventAnotherRelayHasLeased is what keeps two Booking replicas
// from both publishing every event.
func TestRelaySkipsAnEventAnotherRelayHasLeased(t *testing.T) {
	ctx := context.Background()
	aggregateID := enqueue(t, ctx, domain.EventBookingRefunded)
	row := outboxRows(t, ctx, aggregateID)[0]
	if _, err := testPool.Exec(ctx, `UPDATE outbox SET claimed_until = now() + interval '1 hour' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("lease row: %v", err)
	}

	relayAll(t, ctx, events.NewRelay(amqpURL, postgres.NewRepo(testPool), quietLog()))

	if outboxRows(t, ctx, aggregateID)[0].Published {
		t.Fatal("published an event another relay holds a live lease on")
	}
}
