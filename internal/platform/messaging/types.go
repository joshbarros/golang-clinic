// Package messaging is the transactional outbox, the inbox, and the RabbitMQ and Kafka plumbing around them.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Names of the exchanges, the Kafka topic and the routing keys. They are the contract between the modules.
const (
	Exchange          = "clinic.work"
	DeadLetterExch    = "clinic.dlx"
	RequeueExchange   = "clinic.requeue"
	KafkaTopic        = "clinic.events"
	RetryDelayMs      = 2000
	MaxAttempts       = 3
	ReserveKey        = "slots.reserve"
	ReleaseKey        = "slots.release"
	HeldKey           = "slots.held"
	ShortKey          = "slots.short"
	ReleasedKey       = "slots.released"
	IssuedKey         = "letter.issued"
	RejectedKey       = "letter.rejected"
	SessionsQueue     = "scheduling.commands"
	LettersQueue      = "referrals.work"
	AppointmentsQueue = "appointments.events"
	PoisonToken       = "POISON"
	attemptHeader     = "x-attempt"
	outboxBatchSize   = 50
	relayIdleDelay    = 200 * time.Millisecond
	relayErrorDelay   = time.Second
	trimEvery         = 10 * time.Minute
)

// Delivery is one message handed to a Handler.
type Delivery struct {
	MessageID  uuid.UUID
	RoutingKey string
	Body       []byte
}

// Handler consumes one message. Returning ErrPoison dead-letters it at once; any other error retries it.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }

// ErrPoison marks a message whose rejection is already committed: it must be dead-lettered, not retried.
var ErrPoison = errors.New("poison message")

// OutOfAppointmentError means the message arrived before the state it depends on; it is retried later.
type OutOfAppointmentError struct{ Reason string }

func (e *OutOfAppointmentError) Error() string { return e.Reason }

// IsPoison reports whether the reference carries the poison token (a case-sensitive substring).
func IsPoison(reference string) bool { return strings.Contains(reference, PoisonToken) }

// FailureAction is what a consumer does with a message whose handler failed.
type FailureAction int

// The two things a failed message can meet.
const (
	ActionRetry FailureAction = iota
	ActionDeadLetter
)

// Decide: a poison message is dead-lettered at once; anything else is retried until it has been tried MaxAttempts times.
func Decide(priorAttempts int, err error) FailureAction {
	if errors.Is(err, ErrPoison) {
		return ActionDeadLetter
	}
	if priorAttempts+1 >= MaxAttempts {
		return ActionDeadLetter
	}
	return ActionRetry
}

// ---- the messages ----

// ReserveSlot asks the session to hold slots.
type ReserveSlot struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	OwnerID       uuid.UUID `json:"ownerId"`
	SessionID     uuid.UUID `json:"sessionId"`
	Slots         int       `json:"slots"`
	Reference     string    `json:"reference"`
}

// SlotHeld says the slots are held.
type SlotHeld struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	OwnerID       uuid.UUID `json:"ownerId"`
	SessionID     uuid.UUID `json:"sessionId"`
	Slots         int       `json:"slots"`
	Reference     string    `json:"reference"`
}

// SlotShort says the reserve was refused: the remaining slots did not cover it.
type SlotShort struct {
	AppointmentID  uuid.UUID `json:"appointmentId"`
	Reference      string    `json:"reference"`
	RequestedSlots int       `json:"requestedSlots"`
	RemainingSlots int       `json:"remainingSlots"`
}

// ReleaseSlots asks the session to give held slots back.
type ReleaseSlots struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	Reason        string    `json:"reason"`
}

// SlotReleased says slots (possibly none) were given back.
type SlotReleased struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	Slots         int       `json:"slots"`
	Reason        string    `json:"reason"`
}

// LetterIssued says the letter exists.
type LetterIssued struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	Number        string    `json:"number"`
}

// LetterRejected says the letter was refused (the poison rule).
type LetterRejected struct {
	AppointmentID uuid.UUID `json:"appointmentId"`
	Reason        string    `json:"reason"`
}

// Parse decodes a message body.
func Parse[T any](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	return v, nil
}
