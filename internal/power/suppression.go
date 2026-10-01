package power

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// SuppressionKind distinguishes how a command was held back.
type SuppressionKind string

const (
	// SuppressionClamped means the command was applied at a reduced setpoint.
	SuppressionClamped SuppressionKind = "clamped"
	// SuppressionDeferred means the command was held for a later slot (错峰).
	SuppressionDeferred SuppressionKind = "deferred"
)

// SuppressionStatus is the lifecycle of a suppressed command.
type SuppressionStatus string

const (
	SuppressionActive    SuppressionStatus = "active"
	SuppressionRecovered SuppressionStatus = "recovered"
	SuppressionExpired   SuppressionStatus = "expired"
)

// Suppression records one command the governor held back, why, and what has
// to be true before the full request can go through again.
type Suppression struct {
	ID              uuid.UUID         `json:"id"`
	LineID          model.LineID      `json:"line_id"`
	DeviceID        string            `json:"device_id"`
	Kind            SuppressionKind   `json:"kind"`
	Action          string            `json:"action"`
	Generation      uint64            `json:"generation"`
	RequestedOutput float64           `json:"requested_output"`
	AppliedOutput   float64           `json:"applied_output"`
	NeededKW        float64           `json:"needed_kw"`
	Reason          string            `json:"reason"`
	Recovery        string            `json:"recovery"`
	Payload         map[string]string `json:"payload,omitempty"`
	NotBefore       time.Time         `json:"not_before,omitempty"`
	ExpiresAt       time.Time         `json:"expires_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	Status          SuppressionStatus `json:"status"`
	ResolvedAt      *time.Time        `json:"resolved_at,omitempty"`
}

func (s Suppression) active() bool {
	return s.Status == SuppressionActive
}

// suppressionBook is the in-memory ledger of suppressed commands, newest
// clamped record per device+action superseding the older one.
type suppressionBook struct {
	mu      sync.RWMutex
	records map[uuid.UUID]Suppression
}

func newSuppressionBook() *suppressionBook {
	return &suppressionBook{records: make(map[uuid.UUID]Suppression)}
}

// maxSuppressions bounds the ledger; the oldest resolved records are pruned
// first so active suppressions are never dropped.
const maxSuppressions = 512

func (b *suppressionBook) add(record Suppression) Suppression {
	b.mu.Lock()
	defer b.mu.Unlock()
	if record.Kind == SuppressionClamped {
		// A fresher clamp on the same device+action replaces the old record;
		// the control loop will re-issue on its next cycle anyway.
		for id, existing := range b.records {
			if existing.active() && existing.Kind == SuppressionClamped &&
				existing.DeviceID == record.DeviceID && existing.Action == record.Action {
				delete(b.records, id)
			}
		}
	}
	b.records[record.ID] = record
	if len(b.records) > maxSuppressions {
		resolved := make([]Suppression, 0, len(b.records))
		for _, existing := range b.records {
			if !existing.active() {
				resolved = append(resolved, existing)
			}
		}
		sort.Slice(resolved, func(i, j int) bool { return resolved[i].CreatedAt.Before(resolved[j].CreatedAt) })
		for _, existing := range resolved {
			if len(b.records) <= maxSuppressions {
				break
			}
			delete(b.records, existing.ID)
		}
	}
	return record
}

func (b *suppressionBook) resolve(id uuid.UUID, status SuppressionStatus, now time.Time) (Suppression, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.records[id]
	if !ok || !record.active() {
		return Suppression{}, false
	}
	stamp := now.UTC()
	record.Status = status
	record.ResolvedAt = &stamp
	b.records[id] = record
	return record, true
}

func (b *suppressionBook) list(includeResolved bool) []Suppression {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Suppression, 0, len(b.records))
	for _, record := range b.records {
		if !includeResolved && !record.active() {
			continue
		}
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func (b *suppressionBook) activeDeferred(now time.Time) []Suppression {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Suppression, 0)
	for _, record := range b.records {
		if record.active() && record.Kind == SuppressionDeferred {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NotBefore.Before(result[j].NotBefore) })
	return result
}

func (b *suppressionBook) activeClamped() []Suppression {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Suppression, 0)
	for _, record := range b.records {
		if record.active() && record.Kind == SuppressionClamped {
			result = append(result, record)
		}
	}
	return result
}
