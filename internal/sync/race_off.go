//go:build !race

package sync

// raceEnabled marks race-detector builds: the shadow memory roughly
// doubles RSS, so soak budgets widen (see soak_test.go).
const raceEnabled = false
