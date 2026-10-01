package power

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Outcome is the guard verdict for a command.
type Outcome string

const (
	// OutcomeAllowed passes the command through unchanged.
	OutcomeAllowed Outcome = "allowed"
	// OutcomeClamped passes a reduced setpoint that still fits the budget.
	OutcomeClamped Outcome = "clamped"
	// OutcomeDeferred withholds ("peak shaves") the command until power frees
	// up or trustworthy readings return.
	OutcomeDeferred Outcome = "deferred"
	// OutcomeRejected refuses a command that would break minimum aeration or
	// minimum return protection.
	OutcomeRejected Outcome = "rejected"
	// OutcomeBypassed passes an emergency command through without limiting.
	OutcomeBypassed Outcome = "bypassed"
)

// Reason codes carried on verdicts and suppressed-command views.
const (
	ReasonUnknownDevice   = "unknown_device"
	ReasonMinAeration     = "minimum_aeration_protected"
	ReasonMinReturn       = "minimum_return_protected"
	ReasonPeakShaving     = "peak_shaving"
	ReasonReadingsUntrust = "readings_untrusted"
	ReasonLineUnknown     = "line_budget_unknown"
)

// Decision is the result of adjudicating one command.
type Decision struct {
	RequestID uuid.UUID           `json:"request_id"`
	Command   model.DeviceCommand `json:"command"`
	Outcome   Outcome             `json:"outcome"`
	Reason    string              `json:"reason,omitempty"`
	// FinalSetpoint is what the guard allows to be published. For a deferred or
	// rejected command it is zero (a zero final setpoint is still reported).
	FinalSetpoint float64 `json:"final_setpoint"`
	// Headroom is the line picture the verdict was based on.
	Headroom LineHeadroom `json:"headroom"`
	// Recovery describes what must happen before a deferred command can run.
	Recovery  []string  `json:"recovery_conditions,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// SuppressedCommand is a withheld command plus the conditions to release it.
type SuppressedCommand struct {
	RequestID    uuid.UUID           `json:"request_id"`
	Command      model.DeviceCommand `json:"command"`
	Reason       string              `json:"reason"`
	DesiredW     float64             `json:"desired_w"`
	Headroom     LineHeadroom        `json:"headroom"`
	Recovery     []string            `json:"recovery_conditions"`
	SuppressedAt time.Time           `json:"suppressed_at"`
}

// ReleaseFunc publishes a command the guard has allowed or retried. It is
// injected by the plant so the power package stays free of process imports.
type ReleaseFunc func(context.Context, model.DeviceCommand) error

// Manager is the budget guard: it adjudicates commands, remembers the running
// setpoint of each protected device and holds deferred commands until the
// budget or the readings allow them.
type Manager struct {
	mu         sync.Mutex
	calculator *Calculator
	registry   *Registry
	telemetry  *TelemetryBook
	release    ReleaseFunc
	now        func() time.Time

	running map[string]float64
	pending map[string]suppressed
	order   []string // device IDs with a pending request, FIFO
	history []Decision
}

type suppressed struct {
	command      model.DeviceCommand
	desiredW     float64
	reason       string
	suppressedAt time.Time
}

// historyLimit bounds how many recent verdicts the operator page can show.
const historyLimit = 20

// NewManager wires the guard. The release callback may be set later with
// BindReleaser before any command is adjudicated.
func NewManager(calculator *Calculator, registry *Registry, telemetry *TelemetryBook, now func() time.Time) (*Manager, error) {
	if calculator == nil || registry == nil || telemetry == nil || now == nil {
		return nil, errors.New("power manager dependencies are required")
	}
	manager := &Manager{
		calculator: calculator, registry: registry, telemetry: telemetry, now: now,
		running: make(map[string]float64), pending: make(map[string]suppressed),
	}
	for _, lineID := range registry.Lines() {
		for _, profile := range registry.Line(lineID) {
			manager.running[profile.ID] = 0
		}
	}
	return manager, nil
}

// BindReleaser installs the callback used to publish released commands.
func (m *Manager) BindReleaser(release ReleaseFunc) {
	m.mu.Lock()
	m.release = release
	m.mu.Unlock()
}

// Lookup returns the electrical profile for a device.
func (m *Manager) Lookup(deviceID string) (Profile, bool) {
	return m.registry.Device(deviceID)
}

// MarkRunning records the confirmed setpoint of a device (for example a return
// pump confirming flow, or a stop compensation completing).
func (m *Manager) MarkRunning(deviceID string, setpoint float64) {
	m.mu.Lock()
	if _, known := m.running[deviceID]; known {
		m.running[deviceID] = setpoint
	}
	m.mu.Unlock()
}

// Adjudicate gates one command. Emergency commands bypass every limit. The
// returned decision is always populated; a non-allowed outcome is never fatal
// so callers can surface it to the operator page.
func (m *Manager) Adjudicate(ctx context.Context, command model.DeviceCommand, emergency bool) Decision {
	now := m.now().UTC()
	profile, ok := m.registry.Device(command.DeviceID)

	m.mu.Lock()
	defer m.mu.Unlock()

	if !ok || profile.Kind != command.Kind {
		return m.record(Decision{
			Command: command, Outcome: OutcomeRejected, Reason: ReasonUnknownDevice,
			Headroom: m.headroom(command.LineID, now), DecidedAt: now,
		})
	}
	headroom, exists := m.calculator.Line(command.LineID, now)
	if !exists {
		return m.record(Decision{
			Command: command, Outcome: OutcomeRejected, Reason: ReasonLineUnknown,
			DecidedAt: now,
		})
	}

	// Emergency overrides the guard entirely but still publishes so the
	// safety action physically executes.
	if emergency {
		if m.release != nil {
			_ = m.release(ctx, command)
		}
		m.running[command.DeviceID] = command.Setpoint
		return m.record(Decision{
			Command: command, Outcome: OutcomeBypassed,
			FinalSetpoint: command.Setpoint, Headroom: headroom, DecidedAt: now,
		})
	}

	desired := command.Setpoint

	// Protection: minimum aeration and minimum return are non-negotiable.
	if reason, protected := m.protectionReason(profile, desired, headroom.Devices); protected {
		return m.record(Decision{
			Command: command, Outcome: OutcomeRejected, Reason: reason,
			Headroom: headroom, DecidedAt: now,
		})
	}

	desiredPower := profile.PowerW(desired)
	currentPower := devicePower(headroom.Devices, command.DeviceID)
	reduction := desiredPower < currentPower

	// Readings we cannot trust: never invent headroom for an increase.
	// Reaching the protected floor is still allowed so minimum aeration and
	// minimum return can be (re)established without trusting any spare power;
	// reductions are always allowed because they only relieve the line. A
	// second return pump is not part of the protected minimum, so it must wait
	// for trustworthy readings.
	if !headroom.Trusted {
		if reduction || m.mayRunBlind(profile, desiredPower, headroom.Devices) {
			return m.allowLocked(ctx, command, desired, OutcomeAllowed, headroom, now)
		}
		return m.deferLocked(command, desiredPower, ReasonReadingsUntrust, headroom, now)
	}

	postDraw := headroom.CurrentW - currentPower + desiredPower
	if postDraw <= headroom.CapacityW {
		return m.allowLocked(ctx, command, desired, OutcomeAllowed, headroom, now)
	}

	// The command would exceed the contract. Reductions still help even when
	// the line is already over contract, so never block them.
	if reduction {
		return m.allowLocked(ctx, command, desired, OutcomeAllowed, headroom, now)
	}

	if profile.Role == RoleBlower {
		// Clamp the blower to the largest setpoint the remaining budget
		// supports, never below the minimum aeration floor. If the line is so
		// tight that even the floor would not be a raise over the running
		// setpoint, defer instead of forcing the running blower down.
		availableForDevice := headroom.CapacityW - (headroom.CurrentW - currentPower)
		fit := profile.SetpointForPower(availableForDevice)
		if fit < profile.MinSetpoint {
			fit = profile.MinSetpoint // process protection outranks the contract
		}
		runningSetpoint := m.running[command.DeviceID]
		if fit <= runningSetpoint {
			return m.deferLocked(command, desiredPower, ReasonPeakShaving, headroom, now)
		}
		if fit >= desired {
			return m.allowLocked(ctx, command, desired, OutcomeAllowed, headroom, now)
		}
		clamped := command
		clamped.Setpoint = fit
		return m.allowLocked(ctx, clamped, fit, OutcomeClamped, headroom, now)
	}

	// A return pump starts at its protected minimum; it cannot be partially
	// started, so stagger it until the line has room.
	return m.deferLocked(command, desiredPower, ReasonPeakShaving, headroom, now)
}

// mayRunBlind decides whether a command may be allowed when readings are
// untrusted. Only the minimum protected load qualifies: a blower at its floor
// or the first return pump establishing minimum return.
func (m *Manager) mayRunBlind(profile Profile, desiredPower float64, statuses []Status) bool {
	switch profile.Role {
	case RoleBlower:
		return desiredPower <= profile.MinPowerW+0.5
	case RoleReturnPump:
		for _, other := range m.registry.Line(profile.LineID) {
			if other.Role != RoleReturnPump || other.ID == profile.ID {
				continue
			}
			if devicePower(statuses, other.ID) > 0 || m.running[other.ID] > 0 {
				return false
			}
		}
		return true
	}
	return false
}

// protectionReason returns the protection code that forbids the command, if
// any. Stopping or under-driving the only equipment meeting a minimum
// requirement is refused outside an emergency.
func (m *Manager) protectionReason(profile Profile, desired float64, statuses []Status) (string, bool) {
	switch profile.Role {
	case RoleBlower:
		// A blower may never be switched off or driven below the minimum
		// aeration floor in a non-emergency command.
		if desired <= 0 || desired < profile.MinSetpoint {
			return ReasonMinAeration, true
		}
	case RoleReturnPump:
		if desired <= 0 {
			// Another pump on the line must keep the minimum return flowing.
			// Prefer measured draw, fall back to the last commanded setpoint
			// when readings are unavailable.
			for _, other := range m.registry.Line(profile.LineID) {
				if other.Role != RoleReturnPump || other.ID == profile.ID {
					continue
				}
				if devicePower(statuses, other.ID) > 0 || m.running[other.ID] > 0 {
					return "", false
				}
			}
			return ReasonMinReturn, true
		}
	}
	return "", false
}

// allowLocked publishes an allowed command and records the verdict.
func (m *Manager) allowLocked(ctx context.Context, command model.DeviceCommand, setpoint float64, outcome Outcome, headroom LineHeadroom, now time.Time) Decision {
	decision := Decision{
		Command: command, Outcome: outcome, FinalSetpoint: setpoint,
		Headroom: headroom, DecidedAt: now,
	}
	if outcome == OutcomeClamped {
		decision.Command = command
		decision.Reason = ReasonPeakShaving
	}
	if m.release != nil {
		if err := m.release(ctx, command); err != nil {
			// Publication failed: do not pretend the device changed state.
			decision.Outcome = OutcomeDeferred
			decision.Reason = "publish_failed"
			decision.FinalSetpoint = 0
			m.pending[command.DeviceID] = suppressed{
				command: command, desiredW: profilePower(m.registry, command),
				reason: "publish_failed", suppressedAt: now,
			}
			m.enqueueOrder(command.DeviceID)
			return m.record(decision)
		}
	}
	m.running[command.DeviceID] = setpoint
	delete(m.pending, command.DeviceID)
	m.dropOrder(command.DeviceID)
	return m.record(decision)
}

// deferLocked withholds the command and records the live recovery conditions.
func (m *Manager) deferLocked(command model.DeviceCommand, desiredW float64, reason string, headroom LineHeadroom, now time.Time) Decision {
	// A newer request for the same device supersedes an older queued one.
	m.pending[command.DeviceID] = suppressed{
		command: command, desiredW: desiredW, reason: reason, suppressedAt: now,
	}
	m.enqueueOrder(command.DeviceID)
	decision := Decision{
		Command: command, Outcome: OutcomeDeferred, Reason: reason,
		Headroom: headroom, Recovery: m.recoveryLocked(reason, command, headroom), DecidedAt: now,
	}
	return m.record(decision)
}

func (m *Manager) enqueueOrder(deviceID string) {
	m.dropOrder(deviceID)
	m.order = append(m.order, deviceID)
}

func (m *Manager) dropOrder(deviceID string) {
	for index, id := range m.order {
		if id == deviceID {
			m.order = append(m.order[:index], m.order[index+1:]...)
			return
		}
	}
}

func (m *Manager) record(decision Decision) Decision {
	if decision.RequestID == uuid.Nil {
		decision.RequestID = uuid.New()
	}
	m.history = append(m.history, decision)
	if len(m.history) > historyLimit {
		m.history = m.history[len(m.history)-historyLimit:]
	}
	return decision
}

// Reevaluate retries deferred commands in FIFO order against the latest
// readings and budget. It is invoked whenever telemetry arrives or running
// equipment frees power. Returns the decisions for commands released now.
func (m *Manager) Reevaluate(ctx context.Context) []Decision {
	now := m.now().UTC()
	m.mu.Lock()
	candidateIDs := append([]string(nil), m.order...)
	release := m.release
	m.mu.Unlock()

	released := make([]Decision, 0)
	for _, deviceID := range candidateIDs {
		m.mu.Lock()
		item, ok := m.pending[deviceID]
		if !ok {
			m.mu.Unlock()
			continue
		}
		profile, known := m.registry.Device(deviceID)
		headroom, _ := m.calculator.Line(item.command.LineID, now)
		m.mu.Unlock()
		if !known {
			continue
		}
		// Stale/rolled-back readings must keep the command parked.
		if !headroom.Trusted {
			continue
		}
		currentPower := devicePower(headroom.Devices, deviceID)
		postDraw := headroom.CurrentW - currentPower + item.desiredW
		if postDraw > headroom.CapacityW {
			continue
		}
		if release == nil {
			continue
		}
		if err := release(ctx, item.command); err != nil {
			continue
		}
		m.mu.Lock()
		// Confirm the request was not superseded while publishing.
		if current, still := m.pending[deviceID]; still && current.command.ID == item.command.ID {
			m.running[deviceID] = profile.MaxSetpoint
			if profile.Role == RoleBlower {
				m.running[deviceID] = item.command.Setpoint
			}
			delete(m.pending, deviceID)
			m.dropOrder(deviceID)
			released = append(released, m.record(Decision{
				Command: item.command, Outcome: OutcomeAllowed,
				FinalSetpoint: item.command.Setpoint, Headroom: headroom, DecidedAt: m.now().UTC(),
			}))
		}
		m.mu.Unlock()
	}
	return released
}

// CancelLine drops every deferred command for a line, e.g. after an emergency
// isolation renders them meaningless.
func (m *Manager) CancelLine(lineID model.LineID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for deviceID, item := range m.pending {
		if item.command.LineID == lineID {
			delete(m.pending, deviceID)
			m.dropOrder(deviceID)
			removed++
		}
	}
	return removed
}

// recoveryLocked builds the human- and machine-readable resume conditions.
func (m *Manager) recoveryLocked(reason string, command model.DeviceCommand, headroom LineHeadroom) []string {
	profile, _ := m.registry.Device(command.DeviceID)
	switch reason {
	case ReasonReadingsUntrust:
		conditions := []string{}
		for _, problem := range headroom.Problems {
			switch problem {
			case ProblemStale:
				conditions = append(conditions, "等待该处理线设备功率读数刷新（读数超过有效期即视为过期）")
			case ProblemRollback:
				conditions = append(conditions, "电能表读数回卷，需确认计量正常并重新同步后方可恢复")
			case ProblemNoneSeen:
				conditions = append(conditions, "等待设备上报首个有效电表或功率读数")
			}
		}
		if len(conditions) == 0 {
			conditions = append(conditions, "等待可信的功率读数恢复")
		}
		return conditions
	case ReasonPeakShaving:
		need := profile.PowerW(command.Setpoint)
		short := need - headroom.HeadroomW
		return []string{
			fmt.Sprintf("该处理线用电预算不足：本命令约需 %.0f W，当前可信余量 %.0f W（缺口 %.0f W）", need, headroom.HeadroomW, short),
			"等待运行设备释放功率（降低风机频率或回流泵停机）后自动错峰重试",
		}
	case "publish_failed":
		return []string{"命令下发失败，等待下一次遥测触发自动重试"}
	default:
		return []string{"等待预算条件恢复后自动重试"}
	}
}

func (m *Manager) headroom(lineID model.LineID, now time.Time) LineHeadroom {
	headroom, _ := m.calculator.Line(lineID, now)
	return headroom
}

// Suppressed returns every deferred command with its live recovery conditions,
// oldest first, for the operator page.
func (m *Manager) Suppressed() []SuppressedCommand {
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]SuppressedCommand, 0, len(m.order))
	for _, deviceID := range m.order {
		item, ok := m.pending[deviceID]
		if !ok {
			continue
		}
		headroom, _ := m.calculator.Line(item.command.LineID, now)
		result = append(result, SuppressedCommand{
			Command: item.command, Reason: item.reason, DesiredW: item.desiredW,
			Headroom: headroom, Recovery: m.recoveryLocked(item.reason, item.command, headroom),
			SuppressedAt: item.suppressedAt,
		})
	}
	return result
}

// History returns recent verdicts (allowed, clamped, deferred and rejected) in
// decision order.
func (m *Manager) History() []Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Decision(nil), m.history...)
}

// SetpointForPower inverts Profile.PowerW: the largest setpoint whose estimated
// draw does not exceed powerW. Negative budgets clamp to the minimum; budgets
// beyond rated power clamp to the maximum setpoint.
func (p Profile) SetpointForPower(powerW float64) float64 {
	if powerW <= p.MinPowerW {
		return p.MinSetpoint
	}
	if powerW >= p.RatedPowerW {
		return p.MaxSetpoint
	}
	span := p.MaxSetpoint - p.MinSetpoint
	fraction := (powerW - p.MinPowerW) / (p.RatedPowerW - p.MinPowerW)
	return p.MinSetpoint + fraction*span
}

func devicePower(statuses []Status, deviceID string) float64 {
	for _, status := range statuses {
		if status.DeviceID == deviceID {
			return status.PowerW
		}
	}
	return 0
}

func profilePower(registry *Registry, command model.DeviceCommand) float64 {
	profile, ok := registry.Device(command.DeviceID)
	if !ok {
		return 0
	}
	return profile.PowerW(command.Setpoint)
}

// Snapshot exposes the live per-line headroom for status APIs.
func (m *Manager) Snapshot() []LineHeadroom {
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	heads := m.calculator.All(now)
	sort.Slice(heads, func(i, j int) bool { return heads[i].LineID < heads[j].LineID })
	return heads
}
