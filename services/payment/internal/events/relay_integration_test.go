//go:build integration

// Integration tests for Payment's copy of the outbox relay, against a real
// Postgres and a real RabbitMQ. The relay is the same code as Booking's, where
// the lease is also tested; these prove this copy publishes, confirms and waits
// out an outage against Payment's own schema.
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

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/events"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/repository/postgres"
)

var (
	testPool *pgxpool.Pool
	amqpURL  string
)

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	ctx := context.Background()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("payment_db"),
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

func newRelay(url string) *events.Relay {
	return events.NewRelay(url, postgres.NewRepo(testPool),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// enqueue inserts one committed event. Payment writes its events inside the
// settle statement, which needs a charge and a provider to reach; the relay only
// cares that a row is there.
func enqueue(t *testing.T, ctx context.Context, eventType string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	body, err := json.Marshal(domain.Envelope{
		EventID: id, EventType: eventType, OccurredAt: time.Now().UTC(), Version: domain.EventVersion, Payload: map[string]string{},
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO outbox (id, aggregate_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
		id, uuid.Must(uuid.NewV7()), eventType, body); err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
	return id
}

func published(t *testing.T, ctx context.Context, id string) (published, leased bool) {
	t.Helper()
	if err := testPool.QueryRow(ctx,
		`SELECT published_at IS NOT NULL, claimed_until IS NOT NULL FROM outbox WHERE id = $1`, id).Scan(&published, &leased); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return published, leased
}

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

// subscribe binds a private queue to every event, standing in for a consumer.
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

func awaitMessage(t *testing.T, ch *amqp.Channel, queue, id string) amqp.Delivery {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		d, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if d.MessageId == id {
			return d
		}
	}
	t.Fatalf("event %s was never delivered", id)
	return amqp.Delivery{}
}

func TestRelayPublishesCommittedEventsAndMarksThemSent(t *testing.T) {
	ctx := context.Background()
	ch, queue := subscribe(t)
	id := enqueue(t, ctx, domain.EventPaymentSucceeded)

	relayAll(t, ctx, newRelay(amqpURL))

	d := awaitMessage(t, ch, queue, id)
	if d.RoutingKey != domain.EventPaymentSucceeded || d.DeliveryMode != amqp.Persistent {
		t.Fatalf("want a persistent %s, got key %s mode %d", domain.EventPaymentSucceeded, d.RoutingKey, d.DeliveryMode)
	}
	if ok, _ := published(t, ctx, id); !ok {
		t.Fatal("a confirmed event was not marked published")
	}
}

func TestRelayKeepsEventsWhileTheBrokerIsUnreachable(t *testing.T) {
	ctx := context.Background()
	id := enqueue(t, ctx, domain.EventRefundCompleted)

	if _, err := newRelay("amqp://guest:guest@127.0.0.1:1/").RelayOnce(ctx); err == nil {
		t.Fatal("a relay that cannot reach the broker must report it")
	}
	if ok, leased := published(t, ctx, id); ok || leased {
		t.Fatalf("a relay that cannot reach the broker must neither publish nor lease (published=%v leased=%v)", ok, leased)
	}

	ch, queue := subscribe(t)
	relayAll(t, ctx, newRelay(amqpURL))
	awaitMessage(t, ch, queue, id)
	if ok, _ := published(t, ctx, id); !ok {
		t.Fatal("the event that waited out the outage was not marked published")
	}
}
