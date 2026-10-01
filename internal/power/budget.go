package power

import (
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

// LineBudget is the contracted electrical capacity for one process line.
type LineBudget struct {
	LineID model.LineID `json:"line_id"`
	// CapacityW is the total draw the line is allowed to consume under the
	// supply contract.
	CapacityW float64 `json:"capacity_w"`
}

// BudgetBook holds the per-line contracted capacities.
type BudgetBook struct {
	mu      sync.RWMutex
	budgets map[model.LineID]float64
}

// NewBudgetBook builds the book from capacity declarations.
func NewBudgetBook(budgets []LineBudget) (*BudgetBook, error) {
	if len(budgets) == 0 {
		return nil, errBudget("at least one line budget is required")
	}
	book := &BudgetBook{budgets: make(map[model.LineID]float64, len(budgets))}
	for _, budget := range budgets {
		if budget.LineID == "" || budget.CapacityW <= 0 {
			return nil, errBudget("line budget capacity must be positive")
		}
		if _, exists := book.budgets[budget.LineID]; exists {
			return nil, errBudget("duplicate line budget for " + string(budget.LineID))
		}
		book.budgets[budget.LineID] = budget.CapacityW
	}
	return book, nil
}

// Capacity returns the contracted draw for a line.
func (b *BudgetBook) Capacity(lineID model.LineID) (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	capacity, ok := b.budgets[lineID]
	return capacity, ok
}

// DefaultBudgets assigns one shared capacity per line sized for one blower plus
// one return pump plus operating margin.
func DefaultBudgets(lineIDs []model.LineID) []LineBudget {
	budgets := make([]LineBudget, 0, len(lineIDs))
	for index, lineID := range lineIDs {
		// 75 kW per line, gently staggered so adjacent lines are not identical.
		budgets = append(budgets, LineBudget{LineID: lineID, CapacityW: 75000 + float64(index*1000)})
	}
	return budgets
}

// LineHeadroom is the trustworthy budget picture for one process line.
type LineHeadroom struct {
	LineID    model.LineID `json:"line_id"`
	CapacityW float64      `json:"capacity_w"`
	CurrentW  float64      `json:"current_w"`
	// ReservedW is the draw protected by minimum aeration / minimum return
	// requirements; it can never be taken away by the guard.
	ReservedW float64 `json:"reserved_w"`
	// HeadroomW is capacity minus the draw we actually trust. It is meaningful
	// only when Trusted is true.
	HeadroomW float64 `json:"headroom_w"`
	// Trusted is false when any contributing reading is stale, missing or
	// rolled back. HeadroomW is then reported as zero and must not be acted on.
	Trusted bool `json:"trusted"`
	// Problems lists every device reading that prevents trust.
	Problems []ReadingProblem `json:"problems,omitempty"`
	// Devices carries the per-device status behind this summary.
	Devices []Status `json:"devices"`
}

// Calculator joins budgets, profiles and telemetry into per-line headroom.
type Calculator struct {
	budgets   *BudgetBook
	registry  *Registry
	telemetry *TelemetryBook
}

// NewCalculator wires the three stores together.
func NewCalculator(budgets *BudgetBook, registry *Registry, telemetry *TelemetryBook) (*Calculator, error) {
	if budgets == nil || registry == nil || telemetry == nil {
		return nil, errBudget("power calculator dependencies are required")
	}
	return &Calculator{budgets: budgets, registry: registry, telemetry: telemetry}, nil
}

// Line computes the headroom for one line. Untrusted readings collapse the
// reported headroom to zero so callers cannot spend power they cannot verify.
func (c *Calculator) Line(lineID model.LineID, now time.Time) (LineHeadroom, bool) {
	capacity, ok := c.budgets.Capacity(lineID)
	if !ok {
		return LineHeadroom{}, false
	}
	profiles := c.registry.Line(lineID)
	deviceIDs := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		deviceIDs = append(deviceIDs, profile.ID)
	}
	statuses := c.telemetry.Snapshot(deviceIDs, now)

	headroom := LineHeadroom{
		LineID: lineID, CapacityW: capacity,
		Devices: statuses, Problems: make([]ReadingProblem, 0),
	}
	trusted := true
	problemSeen := make(map[ReadingProblem]bool)
	for _, status := range statuses {
		headroom.CurrentW += status.PowerW
		if !status.Trusted {
			trusted = false
			if !problemSeen[status.Problem] {
				problemSeen[status.Problem] = true
				headroom.Problems = append(headroom.Problems, status.Problem)
			}
		}
	}
	headroom.ReservedW = c.reserved(profiles)
	if trusted {
		headroom.HeadroomW = capacity - headroom.CurrentW
		if headroom.HeadroomW < 0 {
			headroom.HeadroomW = 0
		}
	} else {
		// Never advertise spare power on top of data we cannot verify.
		headroom.HeadroomW = 0
	}
	headroom.Trusted = trusted
	sort.Slice(headroom.Problems, func(i, j int) bool { return headroom.Problems[i] < headroom.Problems[j] })
	return headroom, true
}

// reserved sums the draw of the minimum process requirement: every blower at
// its minimum frequency and at least one return pump per line at minimum draw.
func (c *Calculator) reserved(profiles []Profile) float64 {
	var total float64
	pumpsProtected := false
	for _, profile := range profiles {
		switch profile.Role {
		case RoleBlower:
			total += profile.MinPowerW
		case RoleReturnPump:
			if !pumpsProtected {
				total += profile.MinPowerW
				pumpsProtected = true
			}
		}
	}
	return total
}

// All computes headroom for every configured line, sorted by line ID.
func (c *Calculator) All(now time.Time) []LineHeadroom {
	lineIDs := c.registry.Lines()
	result := make([]LineHeadroom, 0, len(lineIDs))
	for _, lineID := range lineIDs {
		if headroom, ok := c.Line(lineID, now); ok {
			result = append(result, headroom)
		}
	}
	return result
}

// reservedPower is a small constructor helper for budget-domain errors.
type budgetError string

func (e budgetError) Error() string { return string(e) }

func errBudget(message string) error { return budgetError(message) }
