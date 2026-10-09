package agent

// LimitsForTest exposes the limits this session currently reads.
func (a *Agent) LimitsForTest() (int, float64) { return a.getLimits().Get() }
