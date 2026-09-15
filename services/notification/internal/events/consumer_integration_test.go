//go:build integration

// Integration tests for the consumer: real RabbitMQ delivering to a notifier
// backed by real Postgres. The mail provider is the only fake, because the
// failures the retry path exists for - a provider refusing everything - are not
// ones a real provider produces on demand, and because counting its calls is
// how "no duplicate email" is observed at all.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/events"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/repository/postgres"
	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/usecase"
)

var (
	testPool *pgxpool.Pool
	amqpURL  string
)

func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	ctx := context.Background()

	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("notif_db"),
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

// countingMailer records every send attempt, and refuses them all when fail is
// set.
type countingMailer struct {
	mu    sync.Mutex
	fail  bool
	calls map[string]int
}

func newMailer(fail bool) *countingMailer {
	return &countingMailer{fail: fail, calls: make(map[string]int)}
}

func (m *countingMailer) Send(_ context.Context, n domain.Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[n.ID]++
	if m.fail {
		return errors.New("simulated provider outage")
	}
	return nil
}

func (m *countingMailer) attempts(notificationID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[notificationID]
}

func (m *countingMailer) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		n += c
	}
	return n
}

// startConsumer runs the real consumer for the length of the test and returns
// once it is consuming. The backoff is short so the retry path runs in
// milliseconds.
func startConsumer(t *testing.T, mailer usecase.Mailer, maxAttempts int) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	notifier := usecase.NewNotifier(postgres.NewRepo(testPool), mailer, maxAttempts, 10*time.Millisecond, log)
	consumer := events.NewConsumer(amqpURL, notifier, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		consumer.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "the consumer to start", consumer.Ready)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func publisher(t *testing.T) *amqp.Channel {
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
	return ch
}

func publish(t *testing.T, ch *amqp.Channel, routingKey, messageID string, body []byte) {
	t.Helper()
	if err := ch.PublishWithContext(context.Background(), events.Exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    messageID,
		Body:         body,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// bookingConfirmed is the envelope Booking publishes when a booking is confirmed,
// written out field by field: it is the contract between the two services.
func bookingConfirmed(t *testing.T, eventID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event_id":    eventID,
		"event_type":  domain.EventBookingConfirmed,
		"occurred_at": time.Now().UTC(),
		"version":     1,
		"payload": map[string]any{
			"booking_id":       uuid.Must(uuid.NewV7()).String(),
			"reservation_id":   uuid.Must(uuid.NewV7()).String(),
			"user_id":          uuid.Must(uuid.NewV7()).String(),
			"concert_event_id": uuid.Must(uuid.NewV7()).String(),
			"total_cents":      5000,
			"tickets": []map[string]string{{
				"ticket_id": uuid.Must(uuid.NewV7()).String(),
				"seat_id":   uuid.Must(uuid.NewV7()).String(),
				"qr_code":   "TKT-" + uuid.Must(uuid.NewV7()).String(),
			}},
		},
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return body
}

// deliveryState reads a notification's delivery record straight from the
// database.
func deliveryState(t *testing.T, ctx context.Context, eventID string) (status string, attempts int, processed bool) {
	t.Helper()
	if err := testPool.QueryRow(ctx, `
		SELECT n.status,
		       (SELECT count(*) FROM delivery_attempts a WHERE a.notification_id = n.id),
		       EXISTS (SELECT 1 FROM processed_events p WHERE p.event_id = n.id)
		FROM notifications n WHERE n.id = $1`, eventID).Scan(&status, &attempts, &processed); err != nil {
		t.Fatalf("read notification %s: %v", eventID, err)
	}
	return status, attempts, processed
}

// awaitDeadLetter waits for a message to arrive in the dead-letter queue.
func awaitDeadLetter(t *testing.T, ch *amqp.Channel, messageID string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		d, ok, err := ch.Get(events.DeadLetterQueue, true)
		if err != nil {
			t.Fatalf("get from dead-letter queue: %v", err)
		}
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if d.MessageId == messageID {
			return
		}
	}
	t.Fatalf("message %s never reached %s", messageID, events.DeadLetterQueue)
}

// TestReplayedEventSendsOneNotification is D10, through the real queue.
//
// The same event arrives twice - exactly what the relay produces when Booking
// dies after the broker confirmed a publish and before the outbox row was marked,
// and what the broker produces when this service dies before acknowledging. The
// customer must get one email.
func TestReplayedEventSendsOneNotification(t *testing.T) {
	ctx := context.Background()
	mailer := newMailer(false)
	startConsumer(t, mailer, 3)
	ch := publisher(t)

	eventID := uuid.Must(uuid.NewV7()).String()
	body := bookingConfirmed(t, eventID)
	publish(t, ch, domain.EventBookingConfirmed, eventID, body)
	publish(t, ch, domain.EventBookingConfirmed, eventID, body)

	// With prefetch 1 the consumer settles messages in order, so once this later
	// event has been sent both copies of the first have been handled.
	sentinel := uuid.Must(uuid.NewV7()).String()
	publish(t, ch, domain.EventBookingConfirmed, sentinel, bookingConfirmed(t, sentinel))
	waitFor(t, "the sentinel event to be sent", func() bool { return mailer.attempts(sentinel) == 1 })

	if n := mailer.attempts(eventID); n != 1 {
		t.Fatalf("D10 violated: one event sent %d emails", n)
	}
	status, attempts, processed := deliveryState(t, ctx, eventID)
	if status != "sent" || attempts != 1 || !processed {
		t.Fatalf("want one sent notification recorded as processed, got status=%s attempts=%d processed=%v", status, attempts, processed)
	}
}

// TestUndeliverableEventIsRetriedThenDeadLettered: a provider that refuses every
// message gets a bounded number of attempts, and then the message moves aside
// instead of blocking the queue or retrying forever.
func TestUndeliverableEventIsRetriedThenDeadLettered(t *testing.T) {
	ctx := context.Background()
	mailer := newMailer(true)
	startConsumer(t, mailer, 3)
	ch := publisher(t)

	eventID := uuid.Must(uuid.NewV7()).String()
	publish(t, ch, domain.EventBookingConfirmed, eventID, bookingConfirmed(t, eventID))
	awaitDeadLetter(t, ch, eventID)

	if n := mailer.attempts(eventID); n != 3 {
		t.Fatalf("want 3 delivery attempts before dead-lettering, got %d", n)
	}
	status, attempts, processed := deliveryState(t, ctx, eventID)
	if status != "failed" || attempts != 3 {
		t.Fatalf("want a failed notification with 3 logged attempts, got status=%s attempts=%d", status, attempts)
	}
	if processed {
		t.Fatal("an undelivered event must not be recorded as processed, or replaying it from the DLQ would send nothing")
	}
}

// TestPoisonMessageIsDeadLetteredWithoutBlockingTheQueue: a message that can
// never be read goes straight to the DLQ, with no attempt, and the event behind
// it is still delivered.
func TestPoisonMessageIsDeadLetteredWithoutBlockingTheQueue(t *testing.T) {
	mailer := newMailer(false)
	startConsumer(t, mailer, 3)
	ch := publisher(t)

	poison := uuid.Must(uuid.NewV7()).String()
	publish(t, ch, domain.EventBookingConfirmed, poison, []byte("this is not an event"))
	next := uuid.Must(uuid.NewV7()).String()
	publish(t, ch, domain.EventBookingConfirmed, next, bookingConfirmed(t, next))

	awaitDeadLetter(t, ch, poison)
	waitFor(t, "the event behind the poison message to be sent", func() bool { return mailer.attempts(next) == 1 })
	if n := mailer.total(); n != 1 {
		t.Fatalf("a poison message must never reach the mail provider: %d sends", n)
	}
}
